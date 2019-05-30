# go-patricia #

**Documentation**: [GoDoc](http://godoc.org/github.com/tchap/go-patricia/patricia)<br />
**Test Coverage**: [![Coverage
Status](https://coveralls.io/repos/tchap/go-patricia/badge.png)](https://coveralls.io/r/tchap/go-patricia)

## About ##

A generic patricia trie (also called radix tree) implemented in Go (Golang).

The patricia trie as implemented in this library enables fast visiting of items
in some particular ways:

1. visit all items saved in the tree,
2. visit all items matching particular prefix (visit subtree), or
3. given a string, visit all items matching some prefix of that string.

`[]byte` type is used for keys, `interface{}` for values.

`Trie` is not thread safe. Synchronize the access yourself.

### State of the Project ###

Apparently some people are using this, so the API should not change often.
Any ideas on how to make the library better are still welcome.

More (unit) testing would be cool as well...

## Usage ##

Import the package from GitHub first.

```go
import "github.com/tchap/go-patricia/patricia"
```

You can as well use gopkg.in thingie:

```go
import "gopkg.in/tchap/go-patricia.v2/patricia"
```

Then you can start having fun.

```go
printItem := func(prefix patricia.Prefix, item patricia.Item) error {
	fmt.Printf("%q: %v\n", prefix, item)
	return nil
}

// Create a new default trie (using the default parameter values).
trie := NewTrie()

// Create a new custom trie.
trie := NewTrie(MaxPrefixPerNode(16), MaxChildrenPerSparseNode(10))

// Insert some items.
trie.Insert(Prefix("Pepa Novak"), 1)
trie.Insert(Prefix("Pepa Sindelar"), 2)
trie.Insert(Prefix("Karel Macha"), 3)
trie.Insert(Prefix("Karel Hynek Macha"), 4)

// Just check if some things are present in the tree.
key := Prefix("Pepa Novak")
fmt.Printf("%q present? %v\n", key, trie.Match(key))
// "Pepa Novak" present? true
key = Prefix("Karel")
fmt.Printf("Anybody called %q here? %v\n", key, trie.MatchSubtree(key))
// Anybody called "Karel" here? true

// Walk the tree in alphabetical order.
trie.Visit(printItem)
// "Karel Hynek Macha": 4
// "Karel Macha": 3
// "Pepa Novak": 1
// "Pepa Sindelar": 2

// Walk a subtree.
trie.VisitSubtree(Prefix("Pepa"), printItem)
// "Pepa Novak": 1
// "Pepa Sindelar": 2

// Modify an item, then fetch it from the tree.
trie.Set(Prefix("Karel Hynek Macha"), 10)
key = Prefix("Karel Hynek Macha")
fmt.Printf("%q: %v\n", key, trie.Get(key))
// "Karel Hynek Macha": 10

// Walk prefixes.
prefix := Prefix("Karel Hynek Macha je kouzelnik")
trie.VisitPrefixes(prefix, printItem)
// "Karel Hynek Macha": 10

// Delete some items.
trie.Delete(Prefix("Pepa Novak"))
trie.Delete(Prefix("Karel Macha"))

// Walk again.
trie.Visit(printItem)
// "Karel Hynek Macha": 10
// "Pepa Sindelar": 2

// Delete a subtree.
trie.DeleteSubtree(Prefix("Pepa"))

// Print what is left.
trie.Visit(printItem)
// "Karel Hynek Macha": 10
```

## License ##

MIT, check the `LICENSE` file.

[![Gittip
Badge](http://img.shields.io/gittip/alanhamlett.png)](https://www.gittip.com/tchap/
"Gittip Badge")
