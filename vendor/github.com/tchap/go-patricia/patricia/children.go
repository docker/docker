// Copyright (c) 2014 The go-patricia AUTHORS
//
// Use of this source code is governed by The MIT License
// that can be found in the LICENSE file.

package patricia

import (
	"fmt"
	"io"
	"sort"
)

type childList interface {
	length() int
	head() *Trie
	add(child *Trie) childList
	remove(b byte)
	replace(b byte, child *Trie)
	next(b byte) *Trie
	walk(prefix *Prefix, visitor VisitorFunc) error
	print(w io.Writer, indent int)
	clone() childList
	total() int
}

type tries []*Trie

func (t tries) Len() int {
	return len(t)
}

func (t tries) Less(i, j int) bool {
	strings := sort.StringSlice{string(t[i].prefix), string(t[j].prefix)}
	return strings.Less(0, 1)
}

func (t tries) Swap(i, j int) {
	t[i], t[j] = t[j], t[i]
}

type sparseChildList struct {
	children tries
}

func newSparseChildList(maxChildrenPerSparseNode int) childList {
	return &sparseChildList{
		children: make(tries, 0, maxChildrenPerSparseNode),
	}
}

func (list *sparseChildList) length() int {
	return len(list.children)
}

func (list *sparseChildList) head() *Trie {
	return list.children[0]
}

func (list *sparseChildList) add(child *Trie) childList {
	// Search for an empty spot and insert the child if possible.
	if len(list.children) != cap(list.children) {
		list.children = append(list.children, child)
		return list
	}

	// Otherwise we have to transform to the dense list type.
	return newDenseChildList(list, child)
}

func (list *sparseChildList) remove(b byte) {
	for i, node := range list.children {
		if node.prefix[0] == b {
			list.children[i] = list.children[len(list.children)-1]
			list.children[len(list.children)-1] = nil
			list.children = list.children[:len(list.children)-1]
			return
		}
	}

	// This is not supposed to be reached.
	panic("removing non-existent child")
}

func (list *sparseChildList) replace(b byte, child *Trie) {
	// Make a consistency check.
	if p0 := child.prefix[0]; p0 != b {
		panic(fmt.Errorf("child prefix mismatch: %v != %v", p0, b))
	}

	// Seek the child and replace it.
	for i, node := range list.children {
		if node.prefix[0] == b {
			list.children[i] = child
			return
		}
	}
}

func (list *sparseChildList) next(b byte) *Trie {
	for _, child := range list.children {
		if child.prefix[0] == b {
			return child
		}
	}
	return nil
}

func (list *sparseChildList) walk(prefix *Prefix, visitor VisitorFunc) error {

	sort.Sort(list.children)

	for _, child := range list.children {
		*prefix = append(*prefix, child.prefix...)
		if child.item != nil {
			err := visitor(*prefix, child.item)
			if err != nil {
				if err == SkipSubtree {
					*prefix = (*prefix)[:len(*prefix)-len(child.prefix)]
					continue
				}
				*prefix = (*prefix)[:len(*prefix)-len(child.prefix)]
				return err
			}
		}

		err := child.children.walk(prefix, visitor)
		*prefix = (*prefix)[:len(*prefix)-len(child.prefix)]
		if err != nil {
			return err
		}
	}

	return nil
}

func (list *sparseChildList) total() int {
	tot := 0
	for _, child := range list.children {
		if child != nil {
			tot = tot + child.total()
		}
	}
	return tot
}

func (list *sparseChildList) clone() childList {
	clones := make(tries, len(list.children), cap(list.children))
	for i, child := range list.children {
		clones[i] = child.Clone()
	}

	return &sparseChildList{
		children: clones,
	}
}

func (list *sparseChildList) print(w io.Writer, indent int) {
	for _, child := range list.children {
		if child != nil {
			child.print(w, indent)
		}
	}
}

type denseChildList struct {
	min         int
	max         int
	numChildren int
	headIndex   int
	children    []*Trie
}

func newDenseChildList(list *sparseChildList, child *Trie) childList {
	var (
		min int = 255
		max int = 0
	)
	for _, child := range list.children {
		b := int(child.prefix[0])
		if b < min {
			min = b
		}
		if b > max {
			max = b
		}
	}

	b := int(child.prefix[0])
	if b < min {
		min = b
	}
	if b > max {
		max = b
	}

	children := make([]*Trie, max-min+1)
	for _, child := range list.children {
		children[int(child.prefix[0])-min] = child
	}
	children[int(child.prefix[0])-min] = child

	return &denseChildList{
		min:         min,
		max:         max,
		numChildren: list.length() + 1,
		headIndex:   0,
		children:    children,
	}
}

func (list *denseChildList) length() int {
	return list.numChildren
}

func (list *denseChildList) head() *Trie {
	return list.children[list.headIndex]
}

func (list *denseChildList) add(child *Trie) childList {
	b := int(child.prefix[0])
	var i int

	switch {
	case list.min <= b && b <= list.max:
		if list.children[b-list.min] != nil {
			panic("dense child list collision detected")
		}
		i = b - list.min
		list.children[i] = child

	case b < list.min:
		children := make([]*Trie, list.max-b+1)
		i = 0
		children[i] = child
		copy(children[list.min-b:], list.children)
		list.children = children
		list.min = b

	default: // b > list.max
		children := make([]*Trie, b-list.min+1)
		i = b - list.min
		children[i] = child
		copy(children, list.children)
		list.children = children
		list.max = b
	}

	list.numChildren++
	if i < list.headIndex {
		list.headIndex = i
	}
	return list
}

func (list *denseChildList) remove(b byte) {
	i := int(b) - list.min
	if list.children[i] == nil {
		// This is not supposed to be reached.
		panic("removing non-existent child")
	}
	list.numChildren--
	list.children[i] = nil

	// Update head index.
	if i == list.headIndex {
		for ; i < len(list.children); i++ {
			if list.children[i] != nil {
				list.headIndex = i
				return
			}
		}
	}
}

func (list *denseChildList) replace(b byte, child *Trie) {
	// Make a consistency check.
	if p0 := child.prefix[0]; p0 != b {
		panic(fmt.Errorf("child prefix mismatch: %v != %v", p0, b))
	}

	// Replace the child.
	list.children[int(b)-list.min] = child
}

func (list *denseChildList) next(b byte) *Trie {
	i := int(b)
	if i < list.min || list.max < i {
		return nil
	}
	return list.children[i-list.min]
}

func (list *denseChildList) walk(prefix *Prefix, visitor VisitorFunc) error {
	for _, child := range list.children {
		if child == nil {
			continue
		}
		*prefix = append(*prefix, child.prefix...)
		if child.item != nil {
			if err := visitor(*prefix, child.item); err != nil {
				if err == SkipSubtree {
					*prefix = (*prefix)[:len(*prefix)-len(child.prefix)]
					continue
				}
				*prefix = (*prefix)[:len(*prefix)-len(child.prefix)]
				return err
			}
		}

		err := child.children.walk(prefix, visitor)
		*prefix = (*prefix)[:len(*prefix)-len(child.prefix)]
		if err != nil {
			return err
		}
	}

	return nil
}

func (list *denseChildList) print(w io.Writer, indent int) {
	for _, child := range list.children {
		if child != nil {
			child.print(w, indent)
		}
	}
}

func (list *denseChildList) clone() childList {
	clones := make(tries, cap(list.children))

	if list.numChildren != 0 {
		clonedCount := 0
		for i := list.headIndex; i < len(list.children); i++ {
			child := list.children[i]
			if child != nil {
				clones[i] = child.Clone()
				clonedCount++
				if clonedCount == list.numChildren {
					break
				}
			}
		}
	}

	return &denseChildList{
		min:         list.min,
		max:         list.max,
		numChildren: list.numChildren,
		headIndex:   list.headIndex,
		children:    clones,
	}
}

func (list *denseChildList) total() int {
	tot := 0
	for _, child := range list.children {
		if child != nil {
			tot = tot + child.total()
		}
	}
	return tot
}
