package ptable

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/petermattis/pebble/crc"
	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/storage"
)

const (
	blockTrailerLen   = 5
	blockHandleMaxLen = 10 + 10
	footerLen         = 1 + 2*blockHandleMaxLen + 4 + 8
	magicOffset       = footerLen - len(magic)
	versionOffset     = magicOffset - 4

	magic = "\xf7\xcf\xf4\x85\xb7\x41\xe2\x88"

	noChecksum     = 0
	checksumCRC32c = 1

	formatVersion = 3

	// The block type gives the per-block compression format.
	// These constants are part of the file format and should not be changed.
	// They are different from the db.Compression constants because the latter
	// are designed so that the zero value of the db.Compression type means to
	// use the default compression (which is snappy).
	noCompressionBlockType     = 0
	snappyCompressionBlockType = 1
)

// blockHandle is the file offset and length of a block.
type blockHandle struct {
	offset, length uint64
}

// decodeBlockHandle returns the block handle encoded at the start of src, as
// well as the number of bytes it occupies. It returns zero if given invalid
// input.
func decodeBlockHandle(src []byte) (blockHandle, int) {
	offset, n := binary.Uvarint(src)
	length, m := binary.Uvarint(src[n:])
	if n == 0 || m == 0 {
		return blockHandle{}, 0
	}
	return blockHandle{offset, length}, n + m
}

func encodeBlockHandle(dst []byte, b blockHandle) int {
	n := binary.PutUvarint(dst, b.offset)
	m := binary.PutUvarint(dst[n:], b.length)
	return n + m
}

// Writer ...
type Writer struct {
	env        *Env
	opts       *db.Options
	writer     io.Writer
	closer     io.Closer
	block      blockWriter
	indexBlock blockWriter
	// offset is the offset (relative to the table start) of the next block to be
	// written.
	offset uint64
	err    error
	// tmp is a scratch buffer, large enough to hold either footerLen bytes,
	// blockTrailerLen bytes, or (5 * binary.MaxVarintLen64) bytes.
	tmp [footerLen]byte
}

var indexColTypes = []ColumnType{ColumnTypeBytes, ColumnTypeInt64}

// NewWriter ...
func NewWriter(f storage.File, env *Env, opts *db.Options) *Writer {
	// TODO(peter): Ensure we use bufio if "f" is not buffered.
	w := &Writer{
		env:    env,
		opts:   opts,
		writer: f,
		closer: f,
	}
	colTypes := make([]ColumnType, len(w.env.Schema))
	for i := range w.env.Schema {
		colTypes[i] = w.env.Schema[i].Type
	}
	w.block.init(colTypes)
	w.indexBlock.init(indexColTypes)
	return w
}

// AddKV adds a row encoded in a key/value pair to the table. The encoded
// column data must match the table schema. Data must be added in sorted order.
func (w *Writer) AddKV(key, value []byte) error {
	if w.err != nil {
		return w.err
	}
	if w.block.cols[0].count == 0 {
		w.addIndex(key)
	}
	w.env.Decode(key, value, nil, &w.block)
	w.maybeFinishBlock()
	return w.err
}

// AddRow adds a row to the table. The columns in the row must match the table
// schema. Data must be added in sorted order.
func (w *Writer) AddRow(row RowReader) error {
	if w.err != nil {
		return w.err
	}
	if w.block.cols[0].count == 0 {
		key, _ := w.env.Encode(row, nil)
		w.addIndex(key)
	}
	w.block.PutRow(row)
	w.maybeFinishBlock()
	return w.err
}

// EstimatedSize ...
func (w *Writer) EstimatedSize() uint64 {
	return w.offset + uint64(w.block.Size()+w.indexBlock.Size())
}

// Close ...
func (w *Writer) Close() (err error) {
	defer func() {
		if w.closer == nil {
			return
		}
		err1 := w.closer.Close()
		if err == nil {
			err = err1
		}
		w.closer = nil
	}()

	if w.err != nil {
		return w.err
	}

	if w.block.cols[0].count > 0 {
		_, err := w.finishBlock(&w.block)
		if err != nil {
			w.err = err
			return w.err
		}
	}

	// Add the dummy final index entry and write the index block.
	w.addIndex(nil)
	indexBlockHandle, err := w.finishBlock(&w.indexBlock)
	if err != nil {
		w.err = err
		return w.err
	}

	// Write the table footer.
	footer := w.tmp[:footerLen]
	for i := range footer {
		footer[i] = 0
	}
	footer[0] = checksumCRC32c
	n := 1
	n += encodeBlockHandle(footer[n:], blockHandle{})
	n += encodeBlockHandle(footer[n:], indexBlockHandle)
	binary.LittleEndian.PutUint32(footer[versionOffset:], formatVersion)
	copy(footer[magicOffset:], magic)
	if _, err := w.writer.Write(footer); err != nil {
		w.err = err
		return w.err
	}

	// Make any future calls to Set or Close return an error.
	w.err = errors.New("pebble/table: writer is closed")
	return nil
}

func (w *Writer) addIndex(key []byte) {
	w.indexBlock.PutBytes(0, key)
	w.indexBlock.PutInt64(1, int64(w.offset))
}

func (w *Writer) maybeFinishBlock() {
	if int(w.block.Size()) < w.opts.GetBlockSize() {
		return
	}
	_, w.err = w.finishBlock(&w.block)
}

func (w *Writer) finishBlock(block *blockWriter) (blockHandle, error) {
	b := block.Finish()
	// Reset the per-block state.
	block.reset()
	return w.writeRawBlock(b, noCompressionBlockType)
}

func (w *Writer) writeRawBlock(b []byte, blockType byte) (blockHandle, error) {
	w.tmp[0] = blockType

	// Calculate the checksum.
	checksum := crc.New(b).Update(w.tmp[:1]).Value()
	binary.LittleEndian.PutUint32(w.tmp[1:5], checksum)

	// Write the bytes to the file.
	if _, err := w.writer.Write(b); err != nil {
		return blockHandle{}, err
	}
	if _, err := w.writer.Write(w.tmp[:5]); err != nil {
		return blockHandle{}, err
	}
	bh := blockHandle{w.offset, uint64(len(b))}
	w.offset += uint64(len(b)) + blockTrailerLen
	return bh, nil
}