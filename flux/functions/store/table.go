package store

//go:generate go run ../../../_tools/tmpl/main.go -i -data=types.tmpldata table.gen.go.tmpl=table.gen.go

import (
	"fmt"

	"github.com/influxdata/flux"
	"github.com/influxdata/flux/execute"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/services/storage"
	"github.com/influxdata/influxdb/tsdb"
)

type table struct {
	bounds execute.Bounds
	key    flux.GroupKey
	cols   []flux.ColMeta

	// cache of the tags on the current series.
	// len(tags) == len(colMeta)
	tags [][]byte
	defs [][]byte

	done chan struct{}

	// The current number of records in memory
	l int

	colBufs []interface{}
	timeBuf []execute.Time

	err error

	empty bool
	more  bool
}

func newTable(
	bounds execute.Bounds,
	key flux.GroupKey,
	cols []flux.ColMeta,
	defs [][]byte,
) table {
	return table{
		bounds:  bounds,
		key:     key,
		tags:    make([][]byte, len(cols)),
		defs:    defs,
		colBufs: make([]interface{}, len(cols)),
		cols:    cols,
		done:    make(chan struct{}),
		empty:   true,
	}
}

func (t *table) Done() chan struct{}  { return t.done }
func (t *table) Key() flux.GroupKey   { return t.key }
func (t *table) Cols() []flux.ColMeta { return t.cols }
func (t *table) RefCount(n int)       {}
func (t *table) Err() error           { return t.err }
func (t *table) Empty() bool          { return t.empty }
func (t *table) Len() int             { return t.l }

func (t *table) Bools(j int) []bool {
	execute.CheckColType(t.cols[j], flux.TBool)
	return t.colBufs[j].([]bool)
}

func (t *table) Ints(j int) []int64 {
	execute.CheckColType(t.cols[j], flux.TInt)
	return t.colBufs[j].([]int64)
}

func (t *table) UInts(j int) []uint64 {
	execute.CheckColType(t.cols[j], flux.TUInt)
	return t.colBufs[j].([]uint64)
}

func (t *table) Floats(j int) []float64 {
	execute.CheckColType(t.cols[j], flux.TFloat)
	return t.colBufs[j].([]float64)
}

func (t *table) Strings(j int) []string {
	execute.CheckColType(t.cols[j], flux.TString)
	return t.colBufs[j].([]string)
}

func (t *table) Times(j int) []execute.Time {
	execute.CheckColType(t.cols[j], flux.TTime)
	return t.colBufs[j].([]execute.Time)
}

// readTags populates b.tags with the provided tags
func (t *table) readTags(tags models.Tags) {
	for j := range t.tags {
		t.tags[j] = t.defs[j]
	}

	if len(tags) == 0 {
		return
	}

	for _, tag := range tags {
		j := execute.ColIdx(string(tag.Key), t.cols)
		t.tags[j] = tag.Value
	}
}

// appendTags fills the colBufs for the tag columns with the tag value.
func (t *table) appendTags() {
	for j := range t.cols {
		v := t.tags[j]
		if v != nil {
			if t.colBufs[j] == nil {
				t.colBufs[j] = make([]string, len(t.cols))
			}
			colBuf := t.colBufs[j].([]string)
			if cap(colBuf) < t.l {
				colBuf = make([]string, t.l)
			} else {
				colBuf = colBuf[:t.l]
			}
			vStr := string(v)
			for i := range colBuf {
				colBuf[i] = vStr
			}
			t.colBufs[j] = colBuf
		}
	}
}

// appendBounds fills the colBufs for the time bounds
func (t *table) appendBounds() {
	bounds := []execute.Time{t.bounds.Start, t.bounds.Stop}
	for j := range []int{startColIdx, stopColIdx} {
		if t.colBufs[j] == nil {
			t.colBufs[j] = make([]execute.Time, len(t.cols))
		}
		colBuf := t.colBufs[j].([]execute.Time)
		if cap(colBuf) < t.l {
			colBuf = make([]execute.Time, t.l)
		} else {
			colBuf = colBuf[:t.l]
		}
		for i := range colBuf {
			colBuf[i] = bounds[j]
		}
		t.colBufs[j] = colBuf
	}
}

func hasPoints(cur tsdb.Cursor) bool {
	if cur == nil {
		return false
	}

	res := false
	switch cur := cur.(type) {
	case tsdb.IntegerArrayCursor:
		a := cur.Next()
		res = a.Len() > 0
	case tsdb.FloatArrayCursor:
		a := cur.Next()
		res = a.Len() > 0
	case tsdb.UnsignedArrayCursor:
		a := cur.Next()
		res = a.Len() > 0
	case tsdb.BooleanArrayCursor:
		a := cur.Next()
		res = a.Len() > 0
	case tsdb.StringArrayCursor:
		a := cur.Next()
		res = a.Len() > 0
	default:
		panic(fmt.Sprintf("unreachable: %T", cur))
	}
	cur.Close()
	return res
}

type tableNoPoints struct {
	table
}

func newTableNoPoints(
	bounds execute.Bounds,
	key flux.GroupKey,
	cols []flux.ColMeta,
	tags models.Tags,
	defs [][]byte,
) *tableNoPoints {
	t := &tableNoPoints{
		table: newTable(bounds, key, cols, defs),
	}
	t.readTags(tags)

	return t
}

func (t *tableNoPoints) Close() {
	if t.done != nil {
		close(t.done)
		t.done = nil
	}
}

func (t *tableNoPoints) Do(f func(flux.ColReader) error) error {
	defer t.Close()

	f(t)

	return t.err
}

type groupTableNoPoints struct {
	table
	gc storage.GroupCursor
}

func newGroupTableNoPoints(
	gc storage.GroupCursor,
	bounds execute.Bounds,
	key flux.GroupKey,
	cols []flux.ColMeta,
	tags models.Tags,
	defs [][]byte,
) *groupTableNoPoints {
	t := &groupTableNoPoints{
		table: newTable(bounds, key, cols, defs),
		gc:    gc,
	}
	t.readTags(tags)

	return t
}

func (t *groupTableNoPoints) Close() {
	if t.gc != nil {
		t.gc.Close()
		t.gc = nil
	}
	if t.done != nil {
		close(t.done)
		t.done = nil
	}
}

func (t *groupTableNoPoints) Do(f func(flux.ColReader) error) error {
	defer t.Close()

	for t.advanceCursor() {
		if err := f(t); err != nil {
			return err
		}
	}

	return t.err
}

func (t *groupTableNoPoints) advanceCursor() bool {
	for t.gc.Next() {
		if hasPoints(t.gc.Cursor()) {
			t.readTags(t.gc.Tags())
			return true
		}
	}
	return false
}
