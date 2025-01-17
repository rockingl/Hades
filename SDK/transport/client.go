package transport

import (
	"bufio"
	"encoding/binary"
	fmt "fmt"
	io "io"
	"sync"

	"github.com/chriskaliX/SDK/clock"
)

type SendHookFunction func(*Record) error

const ElkeidEnv = "SPECIFIED_AGENT_ID"

var _ ITransport = (*Client)(nil)

type ITransport interface {
	SetSendHook(SendHookFunction)
	// inherit hook
	SendElkeid(*Record) error
	SendDebug(*Record) error

	SendRecord(*Record) error
	ReceiveTask() (*Task, error)
	Flush() error
	Close()
}

type Client struct {
	rx     io.ReadCloser
	tx     io.WriteCloser
	reader *bufio.Reader
	writer *bufio.Writer
	rmu    *sync.Mutex
	wmu    *sync.Mutex
	// Hook function for Elkeid
	hook  SendHookFunction
	clock clock.IClock
}

func (c *Client) SetSendHook(hook SendHookFunction) {
	c.hook = hook
}

// Plugin Client send record to agent. Add an extra size flag to simplify
// the operation which agent side decodes.
// Sync With Elkeid
func (c *Client) SendElkeid(rec *Record) (err error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	size := rec.Size()
	err = binary.Write(c.writer, binary.LittleEndian, uint32(size))
	if err != nil {
		return
	}
	var buf []byte
	buf, err = rec.Marshal()
	if err != nil {
		return
	}
	_, err = c.writer.Write(buf)
	return
}

func (c *Client) SendDebug(rec *Record) (err error) {
	fmt.Println(rec.Data.Fields)
	return
}

// Hades send record
func (c *Client) SendRecord(rec *Record) (err error) {
	// fill up with the ts by ticker
	rec.Timestamp = c.clock.Now().Unix()
	// check hook
	if c.hook != nil {
		return c.hook(rec)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var buf []byte
	if buf, err = rec.Marshal(); err != nil {
		return
	}
	if err = binary.Write(c.writer, binary.LittleEndian, uint32(len(buf))); err != nil {
		return
	}
	_, err = c.writer.Write(buf)
	return
}

func (c *Client) ReceiveTask() (t *Task, err error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	var len uint32
	err = binary.Read(c.reader, binary.LittleEndian, &len)
	if err != nil {
		return
	}
	var buf []byte
	buf, err = c.reader.Peek(int(len))
	if err != nil {
		return
	}
	_, err = c.reader.Discard(int(len))
	if err != nil {
		return
	}
	t = &Task{}
	err = t.Unmarshal(buf)
	return
}

func (c *Client) Flush() (err error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.writer.Buffered() != 0 {
		err = c.writer.Flush()
	}
	return
}

func (c *Client) Close() {
	c.writer.Flush()
	c.rx.Close()
	c.tx.Close()
}
