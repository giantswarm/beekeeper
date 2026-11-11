package state

import (
	"io"
	"os"
	"reflect"
)

// readSnapshot decodes an atomic state inode once. Never lend the cached value
// to callers: readers also mutate their copies when planning transactions.
// The descriptor is opened before entering the cache so a rename cannot pair
// the old bytes with the new identity. Writers use the same path under their
// transaction lock and still recover the outbox and enforce the writer fence.
func (s *FileStore) readSnapshot(f *os.File) (*State, error) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !sameSnapshot(s.snapshotFile, info) {
		raw, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		st, err := s.decode(raw)
		if err != nil {
			return nil, err
		}
		s.snapshotFile, s.snapshot = info, st
	}
	return cloneSnapshot(reflect.ValueOf(s.snapshot)).Interface().(*State), nil
}

func sameSnapshot(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Clone the mutable JSON data, including maps, slices and optional pointers.
// Unexported fields (time.Time internals and retained unknown JSON members)
// are immutable opaque values. They are copied with their containing struct;
// decodeKeeping replaces its rest map rather than changing an existing one.
func cloneSnapshot(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(cloneSnapshot(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(cloneSnapshot(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				out.Field(i).Set(cloneSnapshot(v.Field(i)))
			}
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(cloneSnapshot(v.Index(i)))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(cloneSnapshot(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), cloneSnapshot(it.Value()))
		}
		return out
	default:
		return v
	}
}
