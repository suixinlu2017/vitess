/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vstreamer

import (
	"context"
	"fmt"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/schema"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	querypb "vitess.io/vitess/go/vt/proto/query"
)

// RowStreamer exposes an externally usable interface to rowStreamer.
type RowStreamer interface {
	Stream() error
	Cancel()
}

// NewRowStreamer returns a RowStreamer
func NewRowStreamer(ctx context.Context, cp *mysql.ConnParams, se *schema.Engine, query string, lastpk []sqltypes.Value, send func(*binlogdatapb.VStreamRowsResponse) error) RowStreamer {
	return newRowStreamer(ctx, cp, se, query, lastpk, &localVSchema{vschema: &vindexes.VSchema{}}, send)
}

type rowStreamer struct {
	ctx    context.Context
	cancel func()

	cp      *mysql.ConnParams
	se      *schema.Engine
	query   string
	lastpk  []sqltypes.Value
	send    func(*binlogdatapb.VStreamRowsResponse) error
	vschema *localVSchema

	plan      *Plan
	pkColumns []int
	sendQuery string
}

func newRowStreamer(ctx context.Context, cp *mysql.ConnParams, se *schema.Engine, query string, lastpk []sqltypes.Value, vschema *localVSchema, send func(*binlogdatapb.VStreamRowsResponse) error) *rowStreamer {
	ctx, cancel := context.WithCancel(ctx)
	return &rowStreamer{
		ctx:     ctx,
		cancel:  cancel,
		cp:      cp,
		se:      se,
		query:   query,
		lastpk:  lastpk,
		send:    send,
		vschema: vschema,
	}
}

func (rs *rowStreamer) Cancel() {
	rs.cancel()
}

func (rs *rowStreamer) Stream() error {
	// Ensure se is Open. If vttablet came up in a non_serving role,
	// the schema engine may not have been initialized.
	if err := rs.se.Open(); err != nil {
		return err
	}

	if err := rs.buildPlan(); err != nil {
		return err
	}

	conn, err := rs.mysqlConnect()
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecuteFetch("set names binary", 1, false); err != nil {
		return err
	}
	return rs.streamQuery(conn, rs.send)
}

func (rs *rowStreamer) buildPlan() error {
	// This pre-parsing is required to extract the table name
	// and create its metadata.
	_, fromTable, err := analyzeSelect(rs.query)
	if err != nil {
		return err
	}
	st := rs.se.GetTable(fromTable)
	if st == nil {
		return fmt.Errorf("unknown table %v in schema", fromTable)
	}
	ti := &Table{
		Name:    st.Name.String(),
		Columns: st.Columns,
	}
	rs.plan, err = buildTablePlan(ti, rs.vschema, rs.query)
	if err != nil {
		return err
	}
	rs.pkColumns, err = buildPKColumns(st)
	if err != nil {
		return err
	}
	rs.sendQuery, err = rs.buildSelect()
	if err != nil {
		return err
	}
	return err
}

func buildPKColumns(st *schema.Table) ([]int, error) {
	if len(st.PKColumns) == 0 {
		pkColumns := make([]int, len(st.Columns))
		for i := range st.Columns {
			pkColumns[i] = i
		}
		return pkColumns, nil
	}
	for _, pk := range st.PKColumns {
		if pk >= len(st.Columns) {
			return nil, fmt.Errorf("primary key %d refers to non-existent column", pk)
		}
	}
	return st.PKColumns, nil
}

func (rs *rowStreamer) buildSelect() (string, error) {
	buf := sqlparser.NewTrackedBuffer(nil)
	buf.Myprintf("select ")
	prefix := ""
	for _, col := range rs.plan.Table.Columns {
		buf.Myprintf("%s%v", prefix, col.Name)
		prefix = ", "
	}
	buf.Myprintf(" from %v", sqlparser.NewTableIdent(rs.plan.Table.Name))
	if len(rs.lastpk) != 0 {
		if len(rs.lastpk) != len(rs.pkColumns) {
			return "", fmt.Errorf("primary key values don't match length: %v vs %v", rs.lastpk, rs.pkColumns)
		}
		buf.WriteString(" where ")
		prefix := ""
		for lastcol := len(rs.pkColumns) - 1; lastcol >= 0; lastcol-- {
			buf.Myprintf("%s(", prefix)
			prefix = " or "
			for i, pk := range rs.pkColumns[:lastcol] {
				buf.Myprintf("%v = ", rs.plan.Table.Columns[pk].Name)
				rs.lastpk[i].EncodeSQL(buf)
				buf.Myprintf(" and ")
			}
			buf.Myprintf("%v > ", rs.plan.Table.Columns[rs.pkColumns[lastcol]].Name)
			rs.lastpk[lastcol].EncodeSQL(buf)
			buf.Myprintf(")")
		}
	}
	buf.Myprintf(" order by ", sqlparser.NewTableIdent(rs.plan.Table.Name))
	prefix = ""
	for _, pk := range rs.pkColumns {
		buf.Myprintf("%s%v", prefix, rs.plan.Table.Columns[pk].Name)
		prefix = ", "
	}
	return buf.String(), nil
}

func (rs *rowStreamer) streamQuery(conn *mysql.Conn, send func(*binlogdatapb.VStreamRowsResponse) error) error {
	gtid, err := rs.startStreaming(conn)
	if err != nil {
		return err
	}

	// first call the callback with the fields
	flds, err := conn.Fields()
	if err != nil {
		return err
	}
	pkfields := make([]*querypb.Field, len(rs.pkColumns))
	for i, pk := range rs.pkColumns {
		pkfields[i] = &querypb.Field{
			Name: flds[pk].Name,
			Type: flds[pk].Type,
		}
	}

	err = send(&binlogdatapb.VStreamRowsResponse{
		Fields:   rs.plan.fields(),
		Pkfields: pkfields,
		Gtid:     gtid,
	})
	if err != nil {
		return fmt.Errorf("stream send error: %v", err)
	}

	response := &binlogdatapb.VStreamRowsResponse{}
	lastpk := make([]sqltypes.Value, len(rs.pkColumns))
	byteCount := 0
	for {
		select {
		case <-rs.ctx.Done():
			return fmt.Errorf("stream ended: %v", rs.ctx.Err())
		default:
		}

		row, err := conn.FetchNext()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		for i, pk := range rs.pkColumns {
			lastpk[i] = row[pk]
		}
		ok, filtered, err := rs.plan.filter(row)
		if err != nil {
			return err
		}
		if ok {
			response.Rows = append(response.Rows, sqltypes.RowToProto3(filtered))
			for _, s := range filtered {
				byteCount += s.Len()
			}
		}

		if byteCount >= *PacketSize {
			response.Lastpk = sqltypes.RowToProto3(lastpk)
			err = send(response)
			if err != nil {
				return err
			}
			// empty the rows so we start over, but we keep the
			// same capacity
			response.Rows = response.Rows[:0]
			byteCount = 0
		}
	}

	if len(response.Rows) > 0 {
		response.Lastpk = sqltypes.RowToProto3(lastpk)
		err = send(response)
		if err != nil {
			return err
		}
	}

	return nil
}

func (rs *rowStreamer) startStreaming(conn *mysql.Conn) (string, error) {
	lockConn, err := rs.mysqlConnect()
	if err != nil {
		return "", err
	}
	// To be safe, always unlock tables, even if lock tables might fail.
	defer func() {
		_, err := lockConn.ExecuteFetch("unlock tables", 0, false)
		if err != nil {
			log.Warning("Unlock tables failed: %v", err)
		} else {
			log.Infof("Tables unlocked", rs.plan.Table.Name)
		}
		lockConn.Close()
	}()

	log.Infof("Locking table %s for copying", rs.plan.Table.Name)
	if _, err := lockConn.ExecuteFetch(fmt.Sprintf("lock tables %s read", sqlparser.String(sqlparser.NewTableIdent(rs.plan.Table.Name))), 0, false); err != nil {
		return "", err
	}
	pos, err := lockConn.MasterPosition()
	if err != nil {
		return "", err
	}

	if err := conn.ExecuteStreamFetch(rs.sendQuery); err != nil {
		return "", err
	}

	return mysql.EncodePosition(pos), nil
}

func (rs *rowStreamer) mysqlConnect() (*mysql.Conn, error) {
	cp, err := dbconfigs.WithCredentials(rs.cp)
	if err != nil {
		return nil, err
	}
	return mysql.Connect(rs.ctx, cp)
}
