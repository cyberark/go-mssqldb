package mssql

import (
	"encoding/binary"
	"errors"
	"io"
)

type PacketType uint8

type header struct {
	PacketType PacketType
	Status     uint8
	Size       uint16
	Spid       uint16
	PacketNo   uint8
	Pad        uint8
}

// TdsBuffer reads and writes TDS packets of data to the transport.
// The write and read buffers are separate to make sending attn signals
// possible without locks. Currently attn signals are only sent during
// reads, not writes.
type TdsBuffer struct {
	transport io.ReadWriteCloser

	packetSize int

	// Write fields.
	wbuf        []byte
	wpos        int
	wPacketSeq  byte
	wPacketType PacketType

	// Read fields.
	rbuf        []byte
	rpos        int
	rsize       int
	final       bool
	rPacketType PacketType

	// afterFirst is assigned to right after TdsBuffer is created and
	// before the first use. It is executed after the first packet is
	// written and then removed.
	afterFirst func()
}

func NewTdsBuffer(bufsize uint16, transport io.ReadWriteCloser) *TdsBuffer {
	return &TdsBuffer{
		packetSize: int(bufsize),
		wbuf:       make([]byte, 1<<16),
		rbuf:       make([]byte, 1<<16),
		rpos:       8,
		transport:  transport,
	}
}

func (rw *TdsBuffer) ResizeBuffer(packetSize int) {
	rw.packetSize = packetSize
}

func (w *TdsBuffer) PackageSize() int {
	return w.packetSize
}

func (w *TdsBuffer) flush() (err error) {
	// Write packet size.
	w.wbuf[0] = byte(w.wPacketType)
	binary.BigEndian.PutUint16(w.wbuf[2:], uint16(w.wpos))
	w.wbuf[6] = w.wPacketSeq

	// Write packet into underlying transport.
	if _, err = w.transport.Write(w.wbuf[:w.wpos]); err != nil {
		return err
	}
	// It is possible to create a whole new buffer after a flush.
	// Useful for debugging. Normally reuse the buffer.
	// w.wbuf = make([]byte, 1<<16)

	// Execute afterFirst hook if it is set.
	if w.afterFirst != nil {
		w.afterFirst()
		w.afterFirst = nil
	}

	w.wpos = 8
	w.wPacketSeq++
	return nil
}

func (w *TdsBuffer) Write(p []byte) (total int, err error) {
	for {
		copied := copy(w.wbuf[w.wpos:w.packetSize], p)
		w.wpos += copied
		total += copied
		if copied == len(p) {
			return
		}
		if err = w.flush(); err != nil {
			return
		}
		p = p[copied:]
	}
}

func (w *TdsBuffer) WriteByte(b byte) error {
	if int(w.wpos) == len(w.wbuf) || w.wpos == w.packetSize {
		if err := w.flush(); err != nil {
			return err
		}
	}
	w.wbuf[w.wpos] = b
	w.wpos += 1
	return nil
}

func (w *TdsBuffer) BeginPacket(packetType PacketType, resetSession bool) {
	status := byte(0)
	if resetSession {
		switch packetType {
		// Reset session can only be set on the following packet types.
		case PackSQLBatch, PackRPCRequest, PackTransMgrReq:
			status = 0x8
		}
	}
	w.wbuf[1] = status // Packet is incomplete. This byte is set again in FinishPacket.
	w.wpos = 8
	w.wPacketSeq = 1
	w.wPacketType = packetType
}

func (w *TdsBuffer) FinishPacket() error {
	w.wbuf[1] |= 1 // Mark this as the last packet in the message.
	return w.flush()
}

var headerSize = binary.Size(header{})

func (r *TdsBuffer) ReadNextPacket() error {
	h := header{}
	var err error
	err = binary.Read(r.transport, binary.BigEndian, &h)
	if err != nil {
		return err
	}
	if int(h.Size) > r.packetSize {
		return errors.New("Invalid packet size, it is longer than buffer size")
	}
	if headerSize > int(h.Size) {
		return errors.New("Invalid packet size, it is shorter than header size")
	}
	_, err = io.ReadFull(r.transport, r.rbuf[headerSize:h.Size])
	if err != nil {
		return err
	}
	r.rpos = headerSize
	r.rsize = int(h.Size)
	r.final = h.Status != 0
	r.rPacketType = h.PacketType
	return nil
}

func (r *TdsBuffer) BeginRead() (PacketType, error) {
	err := r.ReadNextPacket()
	if err != nil {
		return 0, err
	}
	return r.rPacketType, nil
}

func (r *TdsBuffer) ReadByte() (res byte, err error) {
	if r.rpos == r.rsize {
		if r.final {
			return 0, io.EOF
		}
		err = r.ReadNextPacket()
		if err != nil {
			return 0, err
		}
	}
	res = r.rbuf[r.rpos]
	r.rpos++
	return res, nil
}

func (r *TdsBuffer) byte() byte {
	b, err := r.ReadByte()
	if err != nil {
		badStreamPanic(err)
	}
	return b
}

func (r *TdsBuffer) ReadFull(buf []byte) {
	_, err := io.ReadFull(r, buf[:])
	if err != nil {
		badStreamPanic(err)
	}
}

func (r *TdsBuffer) uint64() uint64 {
	var buf [8]byte
	r.ReadFull(buf[:])
	return binary.LittleEndian.Uint64(buf[:])
}

func (r *TdsBuffer) int32() int32 {
	return int32(r.uint32())
}

func (r *TdsBuffer) uint32() uint32 {
	var buf [4]byte
	r.ReadFull(buf[:])
	return binary.LittleEndian.Uint32(buf[:])
}

func (r *TdsBuffer) uint16() uint16 {
	var buf [2]byte
	r.ReadFull(buf[:])
	return binary.LittleEndian.Uint16(buf[:])
}

func (r *TdsBuffer) BVarChar() string {
	return readBVarCharOrPanic(r)
}

func readBVarCharOrPanic(r io.Reader) string {
	s, err := readBVarChar(r)
	if err != nil {
		badStreamPanic(err)
	}
	return s
}

func readUsVarCharOrPanic(r io.Reader) string {
	s, err := readUsVarChar(r)
	if err != nil {
		badStreamPanic(err)
	}
	return s
}

func (r *TdsBuffer) UsVarChar() string {
	return readUsVarCharOrPanic(r)
}

func (r *TdsBuffer) Read(buf []byte) (copied int, err error) {
	copied = 0
	err = nil
	if r.rpos == r.rsize {
		if r.final {
			return 0, io.EOF
		}
		err = r.ReadNextPacket()
		if err != nil {
			return
		}
	}
	copied = copy(buf, r.rbuf[r.rpos:r.rsize])
	r.rpos += copied
	return
}
