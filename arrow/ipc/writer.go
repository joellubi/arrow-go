// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipc // import "github.com/apache/arrow/go/arrow/ipc"

import (
	"fmt"
	"io"
	"math"

	"github.com/apache/arrow/go/arrow"
	"github.com/apache/arrow/go/arrow/array"
	"github.com/apache/arrow/go/arrow/internal/bitutil"
	"github.com/apache/arrow/go/arrow/memory"
	"github.com/pkg/errors"
)

type swriter struct {
	w   io.Writer
	pos int64
}

func (w *swriter) start() error { return nil }
func (w *swriter) Close() error {
	_, err := w.Write(kEOS[:])
	return err
}

func (w *swriter) write(p payload) error {
	_, err := writeIPCPayload(w, p)
	if err != nil {
		return err
	}
	return nil
}

func (w *swriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.pos += int64(n)
	return n, err
}

// Writer is an Arrow stream writer.
type Writer struct {
	w io.Writer

	mem memory.Allocator
	pw  payloadWriter

	started bool
	schema  *arrow.Schema
}

// NewWriter returns a writer that writes records to the provided output stream.
func NewWriter(w io.Writer, opts ...Option) *Writer {
	cfg := newConfig(opts...)
	return &Writer{
		w:      w,
		mem:    cfg.alloc,
		pw:     &swriter{w: w},
		schema: cfg.schema,
	}
}

func (w *Writer) Close() error {
	if w.pw == nil {
		return nil
	}

	err := w.pw.Close()
	if err != nil {
		return errors.Wrap(err, "arrow/ipc: could not close payload writer")
	}
	w.pw = nil

	return nil
}

func (w *Writer) Write(rec array.Record) error {
	if !w.started {
		err := w.start()
		if err != nil {
			return err
		}
	}

	schema := rec.Schema()
	if schema == nil || !schema.Equal(w.schema) {
		return errInconsistentSchema
	}

	const allow64b = true
	var (
		data = payload{msg: MessageRecordBatch}
		enc  = newRecordEncoder(w.mem, 0, kMaxNestingDepth, allow64b)
	)
	defer data.Release()

	if err := enc.Encode(&data, rec); err != nil {
		return errors.Wrap(err, "arrow/ipc: could not encode record to payload")
	}

	return w.pw.write(data)
}

func (w *Writer) start() error {
	w.started = true

	// write out schema payloads
	ps := payloadsFromSchema(w.schema, w.mem, nil)
	defer ps.Release()

	for _, data := range ps {
		err := w.pw.write(data)
		if err != nil {
			return err
		}
	}

	return nil
}

type recordEncoder struct {
	mem memory.Allocator

	fields []fieldMetadata
	meta   []bufferMetadata

	depth    int64
	start    int64
	allow64b bool
}

func newRecordEncoder(mem memory.Allocator, startOffset, maxDepth int64, allow64b bool) *recordEncoder {
	return &recordEncoder{
		mem:      mem,
		start:    startOffset,
		depth:    maxDepth,
		allow64b: allow64b,
	}
}

func (w *recordEncoder) Encode(p *payload, rec array.Record) error {

	// perform depth-first traversal of the row-batch
	for i, col := range rec.Columns() {
		err := w.visit(p, col)
		if err != nil {
			return errors.Wrapf(err, "arrow/ipc: could not encode column %d (%q)", i, rec.ColumnName(i))
		}
	}

	// position for the start of a buffer relative to the passed frame of reference.
	// may be 0 or some other position in an address space.
	offset := w.start
	w.meta = make([]bufferMetadata, len(p.body))

	// construct the metadata for the record batch header
	for i, buf := range p.body {
		var (
			size    int64
			padding int64
		)
		// the buffer might be null if we are handling zero row lengths.
		if buf != nil {
			size = int64(buf.Len())
			padding = bitutil.CeilByte64(size) - size
		}
		w.meta[i] = bufferMetadata{
			Offset: offset,
			Len:    size + padding,
		}
		offset += size + padding
	}

	p.size = offset - w.start
	if !bitutil.IsMultipleOf8(p.size) {
		panic("not aligned")
	}

	return w.encodeMetadata(p, rec.NumRows())
}

func (w *recordEncoder) visit(p *payload, arr array.Interface) error {
	if w.depth <= 0 {
		return errMaxRecursion
	}

	if !w.allow64b && arr.Len() > math.MaxInt32 {
		return errBigArray
	}

	// add all common elements
	w.fields = append(w.fields, fieldMetadata{
		Len:    int64(arr.Len()),
		Nulls:  int64(arr.NullN()),
		Offset: 0,
	})

	switch arr.NullN() {
	case 0:
		p.body = append(p.body, nil)
	default:
		data := arr.Data()
		bitmap := newTruncatedBitmap(w.mem, int64(data.Offset()), int64(data.Len()), data.Buffers()[0])
		p.body = append(p.body, bitmap)
	}

	switch dtype := arr.DataType().(type) {
	case *arrow.NullType:
		p.body = append(p.body, nil)

	case *arrow.BooleanType:
		data := arr.Data()
		p.body = append(p.body, newTruncatedBitmap(w.mem, int64(data.Offset()), int64(data.Len()), data.Buffers()[1]))

	case arrow.FixedWidthDataType:
		data := arr.Data()
		values := data.Buffers()[1]
		typeWidth := dtype.BitWidth() / 8
		minLength := paddedLength(int64(arr.Len())*int64(typeWidth), kArrowAlignment)

		switch {
		case needTruncate(int64(data.Offset()), values, minLength):
			panic("not implemented") // FIXME(sbinet) writer.cc:212
		default:
			values.Retain()
		}
		p.body = append(p.body, values)

	case *arrow.BinaryType:
		arr := arr.(*array.Binary)
		voffsets, err := w.getZeroBasedValueOffsets(arr)
		if err != nil {
			return errors.Wrapf(err, "could not retrieve zero-based value offsets from %T", arr)
		}
		data := arr.Data()
		values := data.Buffers()[2]

		var totalDataBytes int64
		if voffsets != nil {
			totalDataBytes = int64(len(arr.ValueBytes()))
		}

		switch {
		case needTruncate(int64(data.Offset()), values, totalDataBytes):
			panic("not implemented") // FIXME(sbinet) writer.cc:264
		default:
			values.Retain()
		}
		p.body = append(p.body, voffsets)
		p.body = append(p.body, values)

	case *arrow.StringType:
		arr := arr.(*array.String)
		voffsets, err := w.getZeroBasedValueOffsets(arr)
		if err != nil {
			return errors.Wrapf(err, "could not retrieve zero-based value offsets from %T", arr)
		}
		data := arr.Data()
		values := data.Buffers()[2]

		var totalDataBytes int64
		if voffsets != nil {
			totalDataBytes = int64(arr.ValueOffset(arr.Len()) - arr.ValueOffset(0))
		}

		switch {
		case needTruncate(int64(data.Offset()), values, totalDataBytes):
			panic("not implemented") // FIXME(sbinet) writer.cc:264
		default:
			values.Retain()
		}
		p.body = append(p.body, voffsets)
		p.body = append(p.body, values)

	case *arrow.StructType:
		w.depth--
		arr := arr.(*array.Struct)
		for i := 0; i < arr.NumField(); i++ {
			err := w.visit(p, arr.Field(i))
			if err != nil {
				return errors.Wrapf(err, "could not visit field %d of struct-array", i)
			}
		}
		w.depth++

	case *arrow.ListType:
		arr := arr.(*array.List)
		voffsets, err := w.getZeroBasedValueOffsets(arr)
		if err != nil {
			return errors.Wrapf(err, "could not retrieve zero-based value offsets for array %T", arr)
		}
		p.body = append(p.body, voffsets)

		w.depth--
		var (
			values        = arr.ListValues()
			mustRelease   = false
			values_offset int64
			values_length int64
		)
		defer func() {
			if mustRelease {
				values.Release()
			}
		}()

		if voffsets != nil {
			values_offset = int64(arr.Offsets()[0])
			values_length = int64(arr.Offsets()[arr.Len()]) - values_offset
		}

		if len(arr.Offsets()) != 0 || values_length < int64(values.Len()) {
			// must also slice the values
			values = array.NewSlice(values, values_offset, values_length)
			mustRelease = true
		}
		err = w.visit(p, values)

		if err != nil {
			return errors.Wrapf(err, "could not visit list element for array %T", arr)
		}
		w.depth++

	case *arrow.FixedSizeListType:
		arr := arr.(*array.FixedSizeList)
		voffsets, err := w.getZeroBasedValueOffsets(arr)
		if err != nil {
			return errors.Wrapf(err, "could not retrieve zero-based value offsets for array %T", arr)
		}
		p.body = append(p.body, voffsets)

		w.depth--
		var (
			values        = arr.ListValues()
			mustRelease   = false
			values_offset int64
			values_length int64
		)
		defer func() {
			if mustRelease {
				values.Release()
			}
		}()

		if voffsets != nil {
			values_offset = int64(arr.Offsets()[0])
			values_length = int64(arr.Offsets()[arr.Len()]) - values_offset
		}

		if len(arr.Offsets()) != 0 || values_length < int64(values.Len()) {
			// must also slice the values
			values = array.NewSlice(values, values_offset, values_length)
			mustRelease = true
		}
		err = w.visit(p, values)

		if err != nil {
			return errors.Wrapf(err, "could not visit list element for array %T", arr)
		}
		w.depth++

	default:
		panic(errors.Errorf("arrow/ipc: unknown array %T (dtype=%T)", arr, dtype))
	}

	return nil
}

func (w *recordEncoder) getZeroBasedValueOffsets(arr array.Interface) (*memory.Buffer, error) {
	data := arr.Data()
	voffsets := data.Buffers()[1]
	if data.Offset() != 0 {
		// FIXME(sbinet): writer.cc:231
		panic(fmt.Errorf("not implemented offset=%d", data.Offset()))
	}

	voffsets.Retain()
	return voffsets, nil
}

func (w *recordEncoder) encodeMetadata(p *payload, nrows int64) error {
	p.meta = writeRecordMessage(w.mem, nrows, p.size, w.fields, w.meta)
	return nil
}

func newTruncatedBitmap(mem memory.Allocator, offset, length int64, input *memory.Buffer) *memory.Buffer {
	if input != nil {
		input.Retain()
		return input
	}

	minLength := paddedLength(bitutil.BytesForBits(length), kArrowAlignment)
	switch {
	case offset != 0 || minLength < int64(input.Len()):
		// with a sliced array / non-zero offset, we must copy the bitmap
		panic("not implemented") // FIXME(sbinet): writer.cc:75
	default:
		input.Retain()
		return input
	}
}

func needTruncate(offset int64, buf *memory.Buffer, minLength int64) bool {
	if buf == nil {
		return false
	}
	return offset != 0 || minLength < int64(buf.Len())
}