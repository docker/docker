package tailfile

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

func TestTailFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tail-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	testFile := []byte(`first line
second line
third line
fourth line
fifth line
next first line
next second line
next third line
next fourth line
next fifth line
last first line
next first line
next second line
next third line
next fourth line
next fifth line
next first line
next second line
next third line
next fourth line
next fifth line
last second line
last third line
last fourth line
last fifth line
truncated line`)
	if _, err := f.Write(testFile); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	expected := []string{"last fourth line", "last fifth line"}
	res, err := TailFile(f, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(expected) {
		t.Fatalf("\nexpected:\n%s\n\nactual:\n%s", expected, res)
	}
	for i, l := range res {
		if expected[i] != string(l) {
			t.Fatalf("Expected line %q, got %q", expected[i], l)
		}
	}
}

func TestTailFileManyLines(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tail-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	testFile := []byte(`first line
second line
truncated line`)
	if _, err := f.Write(testFile); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	expected := []string{"first line", "second line"}
	res, err := TailFile(f, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) != len(res) {
		t.Fatalf("\nexpected:\n%s\n\nactual:\n%s", expected, res)
	}
	for i, l := range res {
		if expected[i] != string(l) {
			t.Fatalf("Expected line %s, got %s", expected[i], l)
		}
	}
}

func TestTailEmptyFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tail-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	res, err := TailFile(f, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatal("Must be empty slice from empty file")
	}
}

func TestTailNegativeN(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tail-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	testFile := []byte(`first line
second line
truncated line`)
	if _, err := f.Write(testFile); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := TailFile(f, -1); !errors.Is(err, ErrNonPositiveLinesNumber) {
		t.Fatalf("Expected ErrNonPositiveLinesNumber, got %v", err)
	}
	if _, err := TailFile(f, 0); !errors.Is(err, ErrNonPositiveLinesNumber) {
		t.Fatalf("Expected ErrNonPositiveLinesNumber, got %s", err)
	}
}

func TestNewTailReaderWithDelimiterAfterTruncation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		data        string
		sectionSize int64
		delimiter   string
		reqLines    int
		want        string
		wantLines   int
	}{
		{
			name:        "empty backing",
			sectionSize: blockSize + 7,
			delimiter:   "\n",
			reqLines:    3,
		},
		{
			name:        "large truncation",
			data:        "one\ntwo\npartial",
			sectionSize: 1000 * blockSize,
			delimiter:   "\n",
			reqLines:    10,
			want:        "one\ntwo\n",
			wantLines:   2,
		},
		{
			name:        "partial read limited records",
			data:        "a\nbb\nccc\npartial",
			sectionSize: int64(len("a\nbb\nccc\npartial") + 4),
			delimiter:   "\n",
			reqLines:    2,
			want:        "bb\nccc\n",
			wantLines:   2,
		},
		{
			name:        "short read matching overlap length",
			data:        "###",
			sectionSize: 20,
			delimiter:   "####",
			reqLines:    2,
		},
		{
			name:        "boundary spanning delimiter",
			data:        "aaaa####",
			sectionSize: blockSize + 6,
			delimiter:   "####",
			reqLines:    3,
			want:        "aaaa####",
			wantLines:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			section := io.NewSectionReader(strings.NewReader(tc.data), 0, tc.sectionSize)
			r := &boundedSizeReaderAt{t: t, SectionReader: section}

			tail, lines, err := NewTailReaderWithDelimiter(context.Background(), r, tc.reqLines, []byte(tc.delimiter))
			assert.NilError(t, err)
			assert.Equal(t, lines, tc.wantLines)
			assert.Equal(t, tail.Size(), int64(len(tc.want)))

			got, err := io.ReadAll(tail)
			assert.NilError(t, err)
			assert.Equal(t, string(got), tc.want)
		})
	}
}

func TestNewTailReaderReadError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		data        string
		sectionSize int64
		failRead    int
	}{
		{name: "scan", sectionSize: 2 * blockSize, failRead: 1},
		{name: "end probe", sectionSize: 2 * blockSize, failRead: 2},
		{
			name:     "read ahead",
			data:     "one\n" + strings.Repeat("a", 2*blockSize) + "\n",
			failRead: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wantErr := errors.New("read failed")
			data := strings.NewReader(tc.data)
			var reads int
			r := io.NewSectionReader(readerAtFunc(func(p []byte, off int64) (int, error) {
				reads++
				if reads >= tc.failRead {
					return 0, wantErr
				}
				return data.ReadAt(p, off)
			}), 0, max(tc.sectionSize, data.Size()))

			_, _, err := NewTailReader(context.Background(), r, 1)
			assert.ErrorIs(t, err, wantErr)
		})
	}
}

func TestNewTailReaderCancelEndSearch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := io.NewSectionReader(readerAtFunc(func(p []byte, off int64) (int, error) {
		if ctx.Err() != nil {
			t.Fatal("read after cancellation")
		}
		if len(p) == 1 {
			cancel()
		}
		return 0, io.EOF
	}), 0, 1<<30)

	_, _, err := NewTailReader(ctx, r, 1)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestNewTailReaderTruncateDuringScan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		delimiter string
		surviving string
		suffix    string
		wantLast  string
		wantAll   string
		wantLines int
	}{
		{
			name: "before counting records", delimiter: "\n",
			surviving: "one\ntwo\npartial", suffix: strings.Repeat("x", 4*blockSize),
			wantLast: "two\n", wantAll: "one\ntwo\n", wantLines: 2,
		},
		{
			name: "zero read after counting records", delimiter: "\n",
			surviving: "one\ntwo\npartial", suffix: strings.Repeat("x", 4*blockSize) + "\n",
			wantLast: "two\n", wantAll: "one\ntwo\n", wantLines: 2,
		},
		{
			name: "partial read after counting records", delimiter: "\n",
			surviving: "one\ntwo\npartial", suffix: strings.Repeat("x", blockSize) + "\n",
			wantLast: "two\n", wantAll: "one\ntwo\n", wantLines: 2,
		},
		{
			name: "empty after counting records", delimiter: "\n",
			suffix: strings.Repeat("x", 4*blockSize) + "\n",
		},
		{
			name: "no complete records remain", delimiter: "\n",
			surviving: "partial", suffix: strings.Repeat("x", 4*blockSize) + "\n",
		},
		{
			name: "multibyte delimiter", delimiter: "####",
			surviving: "first####second####partial", suffix: strings.Repeat("x", 4*blockSize) + "####",
			wantLast: "second####", wantAll: "first####second####", wantLines: 2,
		},
	} {
		for _, reqLines := range []int{1, 10} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, reqLines), func(t *testing.T) {
				t.Parallel()
				data := strings.NewReader(tc.surviving + tc.suffix)
				size := data.Size()
				var truncated bool
				r := &boundedSizeReaderAt{
					t: t,
					SectionReader: io.NewSectionReader(readerAtFunc(func(p []byte, off int64) (int, error) {
						n, err := data.ReadAt(p, off)
						if !truncated {
							assert.NilError(t, err)
							assert.Equal(t, n, len(p))
							data = strings.NewReader(tc.surviving)
							truncated = true
						}
						return n, err
					}), 0, size),
				}

				tail, lines, err := NewTailReaderWithDelimiter(context.Background(), r, reqLines, []byte(tc.delimiter))
				assert.NilError(t, err)
				assert.Equal(t, lines, min(reqLines, tc.wantLines))
				want := tc.wantAll
				if reqLines == 1 {
					want = tc.wantLast
				}
				assert.Equal(t, tail.Size(), int64(len(want)))
				got, err := io.ReadAll(tail)
				assert.NilError(t, err)
				assert.Equal(t, string(got), want)
			})
		}
	}
}

type readerAtFunc func([]byte, int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, off int64) (int, error) {
	return f(p, off)
}

type boundedSizeReaderAt struct {
	t *testing.T
	*io.SectionReader
	reads int
}

func (r *boundedSizeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.t.Helper()
	r.reads++
	if r.reads > 100 {
		r.t.Fatal("tail reader did not terminate after source truncation")
	}
	return r.SectionReader.ReadAt(p, off)
}

func BenchmarkTail(b *testing.B) {
	f, err := os.CreateTemp(b.TempDir(), "tail-test")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = f.Close() })
	for range 10000 {
		if _, err := f.WriteString("tailfile pretty interesting line\n"); err != nil {
			b.Fatal(err)
		}
	}

	for b.Loop() {
		if _, err := TailFile(f, 1000); err != nil {
			b.Fatal(err)
		}
	}
}

func TestNewTailReader(t *testing.T) {
	t.Parallel()

	for dName, delim := range map[string][]byte{
		"no delimiter":          {},
		"single byte delimiter": {'\n'},
		"2 byte delimiter":      []byte(";\n"),
		"4 byte delimiter":      []byte("####"),
		"8 byte delimiter":      []byte("########"),
		"12 byte delimiter":     []byte("############"),
	} {
		t.Run(dName, func(t *testing.T) {
			t.Parallel()

			s1 := "Hello world."
			s2 := "Today is a fine day."
			s3 := "So long, and thanks for all the fish!"
			s4 := strings.Repeat("a", blockSize/2) // same as block size
			s5 := strings.Repeat("a", blockSize)   // just to make sure
			s6 := strings.Repeat("a", blockSize*2) // bigger than block size
			s7 := strings.Repeat("a", blockSize-1) // single line same as block

			s8 := `{"log":"Don't panic!\n","stream":"stdout","time":"2018-04-04T20:28:44.7207062Z"}`
			jsonTest := make([]string, 0, 20)
			for range 20 {
				jsonTest = append(jsonTest, s8)
			}

			for _, test := range []struct {
				desc string
				data []string
			}{
				{desc: "one small entry", data: []string{s1}},
				{desc: "several small entries", data: []string{s1, s2, s3}},
				{desc: "various sizes", data: []string{s1, s2, s3, s4, s5, s1, s2, s3, s7, s6}},
				{desc: "multiple lines with one more than block", data: []string{s5, s5, s5, s5, s5}},
				{desc: "multiple lines much bigger than block", data: []string{s6, s6, s6, s6, s6}},
				{desc: "multiple lines same as block", data: []string{s4, s4, s4, s4, s4}},
				{desc: "single line same as block", data: []string{s7}},
				{desc: "single line half block", data: []string{s4}},
				{desc: "single line twice block", data: []string{s6}},
				{desc: "json encoded values", data: jsonTest},
				{desc: "no lines", data: []string{}},
				{desc: "same length as delimiter", data: []string{strings.Repeat("a", len(delim))}},
			} {
				t.Run(test.desc, func(t *testing.T) {
					t.Parallel()

					maxLen := min(len(test.data), 10)

					s := strings.Join(test.data, string(delim))
					if len(test.data) > 0 {
						s += string(delim)
					}

					for i := 1; i <= maxLen; i++ {
						t.Run(fmt.Sprintf("%d lines", i), func(t *testing.T) {
							t.Parallel()

							r := strings.NewReader(s)
							tr, lines, err := NewTailReaderWithDelimiter(t.Context(), r, i, delim)
							if len(delim) == 0 {
								assert.Assert(t, err != nil)
								assert.Assert(t, lines == 0)
								return
							}
							assert.NilError(t, err)
							assert.Check(t, lines == i, "%d -- %d", lines, i)

							b, err := io.ReadAll(tr)
							assert.NilError(t, err)

							expectLines := test.data[len(test.data)-i:]
							assert.Check(t, len(expectLines) == i)
							expect := strings.Join(expectLines, string(delim)) + string(delim)
							assert.Check(t, string(b) == expect, "\n%v\n%v", b, []byte(expect))
						})
					}

					t.Run("request more lines than available", func(t *testing.T) {
						t.Parallel()

						r := strings.NewReader(s)
						tr, lines, err := NewTailReaderWithDelimiter(t.Context(), r, len(test.data)*2, delim)
						if len(delim) == 0 {
							assert.Assert(t, err != nil)
							assert.Assert(t, lines == 0)
							return
						}
						if len(test.data) == 0 {
							assert.Assert(t, errors.Is(err, ErrNonPositiveLinesNumber), err)
							return
						}

						assert.NilError(t, err)
						assert.Check(t, lines == len(test.data), "%d -- %d", lines, len(test.data))
						b, err := io.ReadAll(tr)
						assert.NilError(t, err)
						assert.Check(t, bytes.Equal(b, []byte(s)), "\n%v\n%v", b, []byte(s))
					})
				})
			}
		})
	}
	t.Run("truncated last line", func(t *testing.T) {
		t.Run("more than available", func(t *testing.T) {
			tail, nLines, err := NewTailReader(t.Context(), strings.NewReader("a\nb\nextra"), 3)
			assert.NilError(t, err)
			assert.Check(t, nLines == 2, nLines)

			rdr := bufio.NewReader(tail)
			data, _, err := rdr.ReadLine()
			assert.NilError(t, err)
			assert.Check(t, string(data) == "a", string(data))

			data, _, err = rdr.ReadLine()
			assert.NilError(t, err)
			assert.Check(t, string(data) == "b", string(data))

			_, _, err = rdr.ReadLine()
			assert.Assert(t, errors.Is(err, io.EOF), err)
		})
	})
	t.Run("truncated last line", func(t *testing.T) {
		t.Run("exact", func(t *testing.T) {
			tail, nLines, err := NewTailReader(t.Context(), strings.NewReader("a\nb\nextra"), 2)
			assert.NilError(t, err)
			assert.Check(t, nLines == 2, nLines)

			rdr := bufio.NewReader(tail)
			data, _, err := rdr.ReadLine()
			assert.NilError(t, err)
			assert.Check(t, string(data) == "a", string(data))

			data, _, err = rdr.ReadLine()
			assert.NilError(t, err)
			assert.Check(t, string(data) == "b", string(data))

			_, _, err = rdr.ReadLine()
			assert.Assert(t, errors.Is(err, io.EOF), err)
		})
	})

	t.Run("truncated last line", func(t *testing.T) {
		t.Run("one line", func(t *testing.T) {
			tail, nLines, err := NewTailReader(t.Context(), strings.NewReader("a\nb\nextra"), 1)
			assert.NilError(t, err)
			assert.Check(t, nLines == 1, nLines)

			rdr := bufio.NewReader(tail)
			data, _, err := rdr.ReadLine()
			assert.NilError(t, err)
			assert.Check(t, string(data) == "b", string(data))

			_, _, err = rdr.ReadLine()
			assert.Assert(t, errors.Is(err, io.EOF), err)
		})
	})
}
