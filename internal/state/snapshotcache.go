package state

import (
	"bytes"
	"os"
	"reflect"
	"sync"
)

// readSnapshot decodes an atomic state inode once. Never lend the cached value
// to callers: readers also mutate their copies when planning transactions.
// The descriptor is opened before entering the cache so a rename cannot pair
// the old bytes with the new identity. Writers use the same path under their
// transaction lock and still recover the outbox and enforce the writer fence.
// A replacement is decoded reusing the elements of the last snapshot's arrays
// that it holds unchanged (decodeReusing): a busy desk rewrites the state about
// once a second, and the processes that run as long as the desk would otherwise
// decode all of it every time.
func (s *FileStore) readSnapshot(f *os.File) (*State, error) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !sameSnapshot(s.snapshotFile, info) {
		raw, err := readSized(f, info.Size())
		if err != nil {
			return nil, err
		}
		st, elems, err := s.decodeReusing(raw, s.snapshotElems)
		if err != nil {
			return nil, err
		}
		s.snapshotFile, s.snapshot, s.snapshotRaw, s.snapshotElems = info, st, raw, elems
	}
	return cloneSnapshot(reflect.ValueOf(s.snapshot)).Interface().(*State), nil
}

// readSized reads f to its end, size its expected length: a state document is
// replaced, never changed in place, so one allocation fits it.
func readSized(f *os.File, size int64) ([]byte, error) {
	var b bytes.Buffer
	b.Grow(int(max(size, 0)) + bytes.MinRead)
	_, err := b.ReadFrom(f)
	return b.Bytes(), err
}

// unchangedSnapshot says raw is what state.json holds now, judged by the
// snapshot this store last decoded: a save that changes nothing need not read
// the document back to know it.
func (s *FileStore) unchangedSnapshot(raw []byte) bool {
	info, err := os.Stat(s.path("state.json"))
	if err != nil {
		return false
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	return sameSnapshot(s.snapshotFile, info) && bytes.Equal(s.snapshotRaw, raw)
}

func sameSnapshot(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Clone the mutable JSON data, including maps, slices and optional pointers.
// Unexported fields (time.Time internals and retained unknown JSON members)
// are immutable opaque values. They are copied with their containing struct;
// decodeKeeping replaces its rest map rather than changing an existing one.
// A value whose type holds no map, slice, pointer or interface in its exported
// fields (a string, a time, a struct of such) is copied as it is.
func cloneSnapshot(v reflect.Value) reflect.Value {
	t := v.Type()
	if !holdsReferences(t) {
		return v
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t.Elem())
		out.Elem().Set(cloneSnapshot(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t).Elem()
		out.Set(cloneSnapshot(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for _, i := range referenceFields(t) {
			out.Field(i).Set(cloneSnapshot(v.Field(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(t).Elem()
		for i := range v.Len() {
			out.Index(i).Set(cloneSnapshot(v.Index(i)))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(t, v.Len(), v.Len())
		if !holdsReferences(t.Elem()) {
			reflect.Copy(out, v)
			return out
		}
		for i := range v.Len() {
			out.Index(i).Set(cloneSnapshot(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(t, v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), cloneSnapshot(it.Value()))
		}
		return out
	default:
		return v
	}
}

// cloneTypes caches, per struct type, the indexes of the exported fields that
// hold references, nil for a type that holds none.
var cloneTypes sync.Map // reflect.Type → []int

// holdsReferences says a value of t holds a map, slice, pointer or interface
// that cloneSnapshot copies, in itself or its exported fields at any depth.
func holdsReferences(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
		return true
	case reflect.Array:
		return holdsReferences(t.Elem())
	case reflect.Struct:
		return referenceFields(t) != nil
	default:
		return false
	}
}

// referenceFields are the exported fields of the struct type t that hold
// references; nil when none does.
func referenceFields(t reflect.Type) []int {
	if c, ok := cloneTypes.Load(t); ok {
		return c.([]int)
	}
	// No struct holds itself but through a reference, which holdsReferences
	// answers without looking further: this ends.
	var fields []int
	for i := range t.NumField() {
		if f := t.Field(i); f.IsExported() && holdsReferences(f.Type) {
			fields = append(fields, i)
		}
	}
	cloneTypes.Store(t, fields)
	return fields
}
