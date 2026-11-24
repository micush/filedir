// Package filesync is filedir's replication engine: a folder is scanned into an index of files, each with a version vector and the
// hashes of its blocks; nodes exchange indexes, decide per file which version wins, and fetch only the blocks they lack.  It uses
// only the standard library and knows nothing about the network: peers are reached through the Source interface.
package filesync

import "sort"

// NodeID names a node in version vectors and conflict-file names.
type NodeID string

// Vector is a version vector: how many changes each node has made to a file.  Comparing two vectors says whether one version came
// from the other (one dominates) or whether they were made apart from each other (concurrent), which file times alone can't say.
type Vector map[NodeID]uint64

// Order is how two vectors relate.
type Order int

const (
	Equal      Order = iota
	Greater          // the receiver dominates: it has seen everything the other has, and more
	Lesser           // the other dominates
	Concurrent       // each has changes the other hasn't seen
)

func (o Order) String() string {
	return [...]string{"equal", "greater", "lesser", "concurrent"}[o]
}

// Compare relates v to o.  A missing node counts as 0.
func (v Vector) Compare(o Vector) Order {
	gt, lt := false, false
	for k, a := range v {
		b := o[k]
		if a > b {
			gt = true
		} else if a < b {
			lt = true
		}
	}
	for k, b := range o {
		if _, ok := v[k]; !ok && b > 0 {
			lt = true
		}
	}
	switch {
	case gt && lt:
		return Concurrent
	case gt:
		return Greater
	case lt:
		return Lesser
	}
	return Equal
}

// Copy returns an independent copy (nil for nil).
func (v Vector) Copy() Vector {
	if v == nil {
		return nil
	}
	c := make(Vector, len(v))
	for k, x := range v {
		c[k] = x
	}
	return c
}

// Bump returns v with node n's counter raised by one: n made a change on top of v.
func (v Vector) Bump(n NodeID) Vector {
	c := v.Copy()
	if c == nil {
		c = Vector{}
	}
	c[n]++
	return c
}

// Merge returns the vector that has seen everything v and o have.
func (v Vector) Merge(o Vector) Vector {
	c := v.Copy()
	if c == nil {
		c = Vector{}
	}
	for k, b := range o {
		if b > c[k] {
			c[k] = b
		}
	}
	return c
}

// String is a stable text form, for logs and tests.
func (v Vector) String() string {
	ks := make([]string, 0, len(v))
	for k := range v {
		ks = append(ks, string(k))
	}
	sort.Strings(ks)
	s := "{"
	for i, k := range ks {
		if i > 0 {
			s += " "
		}
		s += k + ":" + itoa(v[NodeID(k)])
	}
	return s + "}"
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
