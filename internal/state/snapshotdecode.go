package state

import (
	"bytes"
	"encoding/json"
	"hash/maphash"
	"reflect"
	"strings"
	"sync"
)

// snapshotElements are the decoded elements of a snapshot's top-level arrays
// (starts, records, agents, deliveries, …), keyed by their bytes in the
// document. A rewrite of a busy desk changes a few of them; decodeReusing
// decodes only those and takes the rest from here, so a long-running reader
// (the loop, the guide's feed, the desktop watch) does not decode the whole
// state for every rewrite. The values are shared between the snapshots that
// hold them, which no caller is lent (readSnapshot clones).
type snapshotElements struct {
	// fields holds, for each splitField, its elements by the hash of their
	// bytes; the bytes are slices of the document they were decoded from.
	fields []map[uint64][]decodedElement
}

type decodedElement struct {
	raw []byte
	v   reflect.Value
}

// splitField is a top-level array of State decoded element by element.
type splitField struct {
	name  string // its JSON name as the state is written
	index int
	typ   reflect.Type // the slice type
}

var (
	elementSeed = maphash.MakeSeed()
	// splitFields are State's slice fields; splitNames their JSON names
	// lower-cased, as encoding/json matches them.
	splitFields = sync.OnceValues(func() ([]splitField, map[string]int) {
		t := reflect.TypeFor[State]()
		var fields []splitField
		names := map[string]int{}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || f.Anonymous || name == "-" || f.Type.Kind() != reflect.Slice || f.Type.Elem().Kind() == reflect.Uint8 {
				continue
			}
			if name == "" {
				name = f.Name
			}
			names[strings.ToLower(name)] = len(fields)
			fields = append(fields, splitField{name: name, index: i, typ: f.Type})
		}
		return fields, names
	})
)

// decodeReusing decodes raw, a state document, as decode does, taking each
// element of a top-level array whose bytes prev holds from prev instead of
// decoding it again. It returns the elements of raw for the next call. A
// document it does not split as the writer writes it (a member named as an
// array in another case or twice, a syntax error) or any error is decoded
// whole by decode, which then gives the error, and no elements are kept.
func (s *FileStore) decodeReusing(raw []byte, prev *snapshotElements) (*State, *snapshotElements, error) {
	if st, next, ok := decodeSplit(raw, prev); ok {
		return st, next, nil
	}
	st, err := s.decode(raw)
	return st, nil, err
}

// decodeSplit is decodeReusing's split decode; ok is false when raw must be
// decoded whole. Each array is scanned once, element by element; the other
// members are decoded together as one object.
func decodeSplit(raw []byte, prev *snapshotElements) (*State, *snapshotElements, bool) {
	fields, names := splitFields()
	type array struct {
		field int
		elems [][]byte
	}
	var arrays []array
	seen := make([]bool, len(fields))
	other := make([]byte, 0, 64<<10)
	other = append(other, '{')
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, nil, false
	}
	for i = skipSpace(raw, i+1); i < len(raw) && raw[i] != '}'; {
		nameStart := i
		end, err := valueEnd(raw, i)
		if err != nil || raw[i] != '"' {
			return nil, nil, false
		}
		name, err := memberName(raw[i:end])
		if err != nil {
			return nil, nil, false
		}
		if i = skipSpace(raw, end); i >= len(raw) || raw[i] != ':' {
			return nil, nil, false
		}
		start := skipSpace(raw, i+1)
		f, split := names[strings.ToLower(name)]
		if split {
			// encoding/json gives a member the field of its name in any
			// case, the last one winning: only the writer's one member, in
			// its case, is split.
			if name != fields[f].name || seen[f] {
				return nil, nil, false
			}
			seen[f] = true
			split = start < len(raw) && raw[start] == '['
		}
		if split {
			var elems [][]byte
			var ok bool
			if elems, end, ok = arrayElements(raw, start); !ok {
				return nil, nil, false
			}
			arrays = append(arrays, array{field: f, elems: elems})
		} else {
			if end, err = valueEnd(raw, start); err != nil {
				return nil, nil, false
			}
			if len(other) > 1 {
				other = append(other, ',')
			}
			other = append(other, raw[nameStart:end]...)
		}
		if i = skipSpace(raw, end); i < len(raw) && raw[i] == ',' {
			i = skipSpace(raw, i+1)
		} else if i >= len(raw) || raw[i] != '}' {
			return nil, nil, false
		}
	}
	if i >= len(raw) || skipSpace(raw, i+1) != len(raw) {
		return nil, nil, false
	}
	other = append(other, '}')
	st := &State{}
	if err := json.Unmarshal(other, st); err != nil {
		return nil, nil, false
	}
	next := &snapshotElements{fields: make([]map[uint64][]decodedElement, len(fields))}
	sv := reflect.ValueOf(st).Elem()
	for _, a := range arrays {
		f := fields[a.field]
		var old map[uint64][]decodedElement
		if prev != nil {
			old = prev.fields[a.field]
		}
		kept := make(map[uint64][]decodedElement, len(a.elems))
		slice := reflect.MakeSlice(f.typ, len(a.elems), len(a.elems))
		for n, b := range a.elems {
			h := maphash.Bytes(elementSeed, b)
			v, found := reflect.Value{}, false
			for _, e := range old[h] {
				if bytes.Equal(e.raw, b) {
					v, found = e.v, true
					break
				}
			}
			if !found {
				p := reflect.New(f.typ.Elem())
				if err := json.Unmarshal(b, p.Interface()); err != nil {
					return nil, nil, false
				}
				v = p.Elem()
			}
			slice.Index(n).Set(v)
			kept[h] = append(kept[h], decodedElement{raw: b, v: v})
		}
		sv.Field(f.index).Set(slice)
		next.fields[a.field] = kept
	}
	return st, next, true
}

// arrayElements are the bytes of each element of the array that starts at
// raw[start], and the index just past it.
func arrayElements(raw []byte, start int) ([][]byte, int, bool) {
	var elems [][]byte
	i := skipSpace(raw, start+1)
	for i < len(raw) && raw[i] != ']' {
		e, err := valueEnd(raw, i)
		if err != nil || e == i {
			return nil, 0, false
		}
		elems = append(elems, raw[i:e])
		if i = skipSpace(raw, e); i < len(raw) && raw[i] == ',' {
			i = skipSpace(raw, i+1)
		} else if i >= len(raw) || raw[i] != ']' {
			return nil, 0, false
		}
	}
	if i >= len(raw) {
		return nil, 0, false
	}
	return elems, i + 1, true
}
