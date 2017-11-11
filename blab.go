/*
Package blab provides a means of creating a network of methods/functions.

Callers include producers and consumers, producers may also connect to other producers.
This first Caller to "Listen" doubles as a broker to direct all Calls.

Once a Call is placed the first Caller/Listener will route the call to the appropriate producer who registered the method or function.

blab has similar requirements for exporting functions that Go's native "rpc" package provides, however blab maps both functions and object methods.

	1)the method or function requires two arguments, both exported (or builtin) types.
	2)the method or function's second argument is a pointer.
	3)the method or function has a return type of error.

Registered methods or functions should look like:

func (*T) Name(argType T1, replyType *T2) error

	and respectively ...

func name(argType T1, replyType *T2) error

Exported functions & methods should be made thread safe.

*/
package blab

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var ErrFail = errors.New("Request failed, service unavailable.")
var ErrClosed = errors.New("Connection closed.")

// Enable Communication Debugging.
var Debug = false

// Limit maximum number of concurrent connections/processes.
var ConnectionLimit = 256

// Message Packet.
type msg struct {
	Dst string
	Src string
	Tag int32
	Err string
	Va1 []byte
	Va2 []byte
}

// The Caller object is for both producers(registering methods/functions) and consumers(calls registered methods/functions).
type Caller struct {
	*session
	fork    bool
	limiter chan struct{}
}

// IPC Session.
type session struct {
	// the socket file.
	socketf string
	// uplink is used to designate our dispatcher.
	uplink *connection
	// log is used for logging errors generated from background routines.
	log *log.Logger
	// reqMap is for outbound request handling.
	reqMap     map[int32]*bucket
	reqMapLock sync.RWMutex
	// localMap is for local methods/function lookup and execution.
	localMap     map[string]func(*msg) *msg
	localMapLock sync.RWMutex
	// busyMap is so a Caller can request the status of a function already being fulfilled.
	busyMap     map[string]map[int32]struct{}
	busyMapLock sync.RWMutex
	// connMap keeps track of all routes that we can send from, if not matched here, send to uplink if avaialble, send Err if not.
	connMap     map[string]*connection
	connMapLock sync.RWMutex
	// peerLock is used to prevent multiple peer connections going to the same location.
	peerLock int32
	// ServeNode
	serveNode *Caller
	ready     uint32
}

// IPC Connection.
type connection struct {
	conn     net.Conn
	enc      *json.Encoder
	sess     *session
	id       string
	routes   []string
	sendLock sync.Mutex
}

// Decodes msg.
func decMessage(in []byte) (out *msg, err error) {

	msgPart := strings.Split(string(in), "\x1f")
	if len(msgPart) < 6 {
		return nil, fmt.Errorf("Incomplete or corrupted message: %s", string(in))
	}

	out = &msg{
		Dst: msgPart[0],
		Src: msgPart[1],
		Err: msgPart[2],
	}

	tag, err := strconv.ParseInt(msgPart[3], 0, 32)
	if err != nil {
		return
	}
	out.Tag = int32(tag)

	va1, err := base64.StdEncoding.DecodeString(msgPart[4])
	if err != nil {
		return
	}
	out.Va1 = va1

	va2, err := base64.StdEncoding.DecodeString(msgPart[5])
	if err != nil {
		return
	}
	out.Va2 = va2

	return
}

// Allocates new Caller.
func NewCaller() *Caller {
	limiter := make(chan struct{}, ConnectionLimit)
	for i := 0; i < ConnectionLimit; i++ {
		limiter <- struct{}{}
	}
	return &Caller{
		newSession(),
		true,
		limiter,
	}
}

// Creates a new blab session.
func newSession() *session {
	return &session{
		uplink:   nil,
		log:      log.New(os.Stdout, "", log.LstdFlags),
		reqMap:   make(map[int32]*bucket),
		localMap: make(map[string]func(*msg) *msg),
		busyMap:  make(map[string]map[int32]struct{}),
		connMap:  make(map[string]*connection),
	}
}

// Directs error output to a specified io.Writer, defaults to os.Stdout.
func (cl *Caller) SetOutput(w io.Writer) {
	if cl.log == nil {
		*cl = *NewCaller()
	}
	cl.log = log.New(w, "", 0)
	if cl.serveNode != nil {
		cl.serveNode.log = cl.log
	}
	return
}

// Listens to socket files(socketf) for Callers.
// If socketf is not open, Listen opens the file and connects itself to it. (producers)
func (cl *Caller) Listen(socketf string) (err error) {
	cl.fork = false

	// Attempt to open socket file, if this works, stop here and serve.
	err = cl.open(socketf)
	if err == nil || !strings.Contains(err.Error(), "connection refused") && !strings.Contains(err.Error(), "no such file or directory") {
		return err
	}

	// If not, we need to continue onward and setup a new socket file.
	if cl.session == nil {
		*cl = *NewCaller()
	}

	server := NewCaller()

	server.socketf = socketf
	server.log = cl.log
	cl.socketf = socketf

	// Clean out old socket files.
	s_split := strings.Split(socketf, "/")
	if len(s_split) == 0 {
		return fmt.Errorf("%s: incomplete path to socket file.", socketf)
	}
	sfile_name := s_split[len(s_split)-1]
	path := strings.Join(s_split[0:len(s_split)-1], "/")

	files, err := ioutil.ReadDir(path)
	if err != nil {
		return
	}
	for _, file := range files {
		fname := file.Name()
		if strings.Contains(fname, sfile_name) {
			os.Remove(path + "/" + fname)
		}
	}

	l, err := net.Listen("unix", socketf)
	if err != nil {
		return err
	}

	cl.serveNode = server

	server.localMapLock.Lock()
	cl.localMapLock.RLock()
	for name, _ := range cl.localMap {
		server.localMap[name] = cl.localMap[name]
	}
	server.localMapLock.Unlock()
	cl.localMapLock.RUnlock()

	for {
		<-cl.limiter
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		c := server.addconnection(conn)

		// Spin connection off to go thread.
		go func() {
			err = c.reciever()
			if err != ErrClosed {
				if Debug {
					cl.log.Println(err)
				}
				return
			}
			cl.limiter <- struct{}{}
		}()
	}
}

// Creates socket file(socketf) connection to Broker, forks goroutine and returns. (consumers)
func (cl *Caller) Dial(socketf string) error {
	cl.fork = true
	return cl.open(socketf)
}

// Creates socket connection to file(socketf) launches listener, forks goroutine or blocks depending on method wrapper called.
func (cl *Caller) open(socketf string) error {
	if cl.session == nil {
		*cl = *NewCaller()
	}

	conn, err := net.Dial("unix", socketf)
	if err != nil {
		return err
	}
	c := cl.addconnection(conn)

	cl.socketf = socketf
	cl.uplink = c

	var done uint32
	atomic.StoreUint32(&done, 1)

	// If this is a service, we'll return the actual listener, if not push to background.
	if !cl.fork {
		return c.reciever()
	} else {
		go func() {
			if err := c.reciever(); err != nil && err != ErrClosed {
				cl.log.Println(err)
			}
		}()
		return nil
	}
}

// Finds split in message when two messages are concatinated together.
func findSplit(in []byte) (n int) {
	for _, ch := range in {
		if ch == '\x04' {
			return n
		}
		n++
	}
	return n
}

// Listens to *connection, decodes msg's and passes them to switchboard.
func (c *connection) reciever() (err error) {
	inbuf := make([]byte, 1024)
	input := inbuf[0:]

	var sz int
	var pbuf []byte

	// Register all local functions with uplink or peer.

	data, _ := json.Marshal(myAddr)
	c.send(&msg{
		Tag: regSelf,
		Va1: data,
	})

	if c.sess.uplink != nil {
		c.sess.localMapLock.RLock()
		for name, _ := range c.sess.localMap {
			data, _ := json.Marshal(name)
			c.send(&msg{
				Src: myAddr,
				Tag: regAddr,
				Va1: data,
			})
		}
		c.sess.localMapLock.RUnlock()
	}
	atomic.StoreUint32(&c.sess.ready, 1)
	// Reciever loop for incoming messages.
	for {
		for n, _ := range input {
			input[n] = 0
		}
		input = inbuf[0:]

		sz, err = c.conn.Read(input)
		if err != nil {
			c.close()
			if err == io.EOF {
				err = ErrClosed
			}
			return
		}

		pbuf = append(pbuf, input[0:sz]...)

		// \x1f used as a delimeter between messages.
		for bytes.Contains(pbuf, []byte("\x04")) {
			//sz := len(pbuf)

			s := findSplit(pbuf)

			var request *msg

			request, err = decMessage(pbuf[0:s])
			if err != nil {
				c.close()
				return
			}
			if Debug {
				switch request.Tag {
				case regAddr:
					fmt.Printf("Recv: [%s] Registering Function: %s\n", c.id, request.Va1)
				case regSelf:
					fmt.Printf("Recv: Received registration from %s.\n", request.Va1[1:len(request.Va1)-1])
				default:
					fmt.Printf("Recv: [%s] Src: %s Dst: %s Tag: %d Err: %s \n", c.id, request.Src, request.Dst, request.Tag, request.Err)
				}
			}
			c.sess.switchboard(c, request)

			if len(pbuf)-s > 1 {
				pbuf = pbuf[s+1:]
				continue
			}
			pbuf = nil
		}
	}
	return
}
