package machine

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aglyzov/log15"
	"github.com/gorilla/websocket"
	"os"
)

var Log = log15.New("pkg", "machine")

type (
	State   byte
	Command byte
	Machine struct {
		URL               string
		Headers           http.Header
		Input             <-chan []byte
		Output            chan<- []byte
		Status            <-chan Status
		Command           chan<- Command
		ReconnectInterval string
		KeepAliveInterval string
	}
	Status struct {
		State State
		Error error
	}
)

const (
	// states
	Disconnected State = iota
	Connecting
	Connected
	Waiting
)
const (
	// commands
	Quit Command = 16 + iota
	Ping
	UseText
	UseBinary
)

const (
	reconnectInterval = "34s"
	keepaliveInterval = "34s"
)

var wg sync.WaitGroup
var inputChannel chan []byte
var outputChannel chan []byte
var statusChannel chan Status
var commandChannel chan Command

var connReturnChannel chan *websocket.Conn
var connCancelChannel chan bool

var readErrorChannel chan error
var writeErrorChannel chan error
var writeControlChannel chan Command

var ioEventChannel chan bool

func init() {
	// disable the logger by default
	Log.SetHandler(log15.DiscardHandler())
}

func (s State) String() string {
	switch s {
	case Disconnected:
		return "Disconnected"
	case Connecting:
		return "Connecting"
	case Connected:
		return "Connected"
	case Waiting:
		return "Waiting"
	}
	return "Unknown status"
}

func (c Command) String() string {
	switch c {
	case Quit:
		return "Quit"
	case Ping:
		return "Ping"
	case UseText:
		return "UseText"
	case UseBinary:
		return "UseBinary"
	}
	return "Unknown command"
}

func New(url string, headers http.Header) *Machine {
	inputChannel = make(chan []byte, 8)
	outputChannel = make(chan []byte, 8)
	statusChannel = make(chan Status, 2)
	commandChannel = make(chan Command, 2)

	connReturnChannel = make(chan *websocket.Conn, 1)
	connCancelChannel = make(chan bool, 1)

	readErrorChannel = make(chan error, 1)
	writeErrorChannel = make(chan error, 1)
	writeControlChannel = make(chan Command, 1)

	ioEventChannel = make(chan bool, 2)

	return &Machine{url, headers, inputChannel, outputChannel, statusChannel, commandChannel, reconnectInterval, keepaliveInterval}
}

func (m *Machine) SetLogHandler(handler log15.Handler) {
	Log.SetHandler(handler)
}

func (m *Machine) Start() {
	go func() {
		// local state
		var conn *websocket.Conn
		reading := false
		writing := false
		messageType := websocket.BinaryMessage // use Binary messages by default

		defer func() {
			Log.Debug("cleanup has started")
			if conn != nil {
				conn.Close()
			} // this also makes reader to exit

			// close local output channels
			close(connCancelChannel)   // this makes connect to exit
			close(writeControlChannel) // this makes write to exit
			close(ioEventChannel)      // this makes keepAlive to exit

			// drain input channels
			<-time.After(50 * time.Millisecond) // small pause to let things react

		drainLoop:
			for {
				select {
				case _, ok := <-outputChannel:
					if !ok {
						outputChannel = nil
					}
				case _, ok := <-commandChannel:
					if !ok {
						inputChannel = nil
					}
				case conn, ok := <-connReturnChannel:
					if conn != nil {
						conn.Close()
					}
					if !ok {
						connReturnChannel = nil
					}
				case _, ok := <-readErrorChannel:
					if !ok {
						readErrorChannel = nil
					}
				case _, ok := <-writeErrorChannel:
					if !ok {
						writeErrorChannel = nil
					}
				default:
					break drainLoop
				}
			}

			// wait for all goroutines to stop
			wg.Wait()

			// close output channels
			close(inputChannel)
			close(statusChannel)
		}()

		Log.Debug("main loop has started")

		go m.connect()
		go m.keepAlive()

	mainLoop:
		for {
			select {
			case conn = <-connReturnChannel:
				if conn == nil {
					break mainLoop
				}
				Log.Debug("connected", "local", conn.LocalAddr(), "remote", conn.RemoteAddr())
				reading = true
				writing = true
				go read(conn)
				go write(conn, messageType)

			case err := <-readErrorChannel:
				reading = false
				if writing {
					// write goroutine is still active
					Log.Debug("read error -> stopping write")
					writeControlChannel <- Quit // ask write to exit
					statusChannel <- Status{Disconnected, err}
				} else {
					// both read and write goroutines have exited
					Log.Debug("read error -> starting connect()")
					if conn != nil {
						conn.Close()
						conn = nil
					}
					go m.connect()
				}
			case err := <-writeErrorChannel:
				// write goroutine has exited
				writing = false
				if reading {
					// read goroutine is still active
					Log.Debug("write error -> stopping read")
					if conn != nil {
						conn.Close() // this also makes read to exit
						conn = nil
					}
					statusChannel <- Status{Disconnected, err}
				} else {
					// both read and write goroutines have exited
					Log.Debug("write error -> starting connect()")
					go m.connect()
				}
			case cmd, ok := <-commandChannel:
				if ok {
					Log.Debug("received command", "cmd", cmd)
				}
				switch {
				case !ok || cmd == Quit:
					if reading || writing || conn != nil {
						statusChannel <- Status{Disconnected, nil}
					}
					break mainLoop // defer should clean everything up
				case cmd == Ping:
					if conn != nil && writing {
						writeControlChannel <- cmd
					}
				case cmd == UseText:
					messageType = websocket.TextMessage
					if writing {
						writeControlChannel <- cmd
					}
				case cmd == UseBinary:
					messageType = websocket.BinaryMessage
					if writing {
						writeControlChannel <- cmd
					}
				default:
					panic(fmt.Sprintf("unsupported command: %v", cmd))
				}
			}
		}
	}()
}

func read(conn *websocket.Conn) {
	wg.Add(1)
	defer wg.Done()

	Log.Debug("read has started")

	for {
		if _, msg, err := conn.ReadMessage(); err == nil {
			Log.Debug("received message", "msg", string(msg))
			ioEventChannel <- true
			inputChannel <- msg
		} else {
			Log.Debug("read error", "err", err)
			readErrorChannel <- err
			break
		}
	}
}

func write(conn *websocket.Conn, messageType int) {
	wg.Add(1)
	defer wg.Done()

	Log.Debug("write has started")

loop:
	for {
		select {
		case msg, ok := <-outputChannel:
			if ok {
				ioEventChannel <- true
				if err := conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
					writeErrorChannel <- err
					break loop
				}
				if err := conn.WriteMessage(messageType, msg); err != nil {
					writeErrorChannel <- err
					break loop
				}
				conn.SetWriteDeadline(time.Time{}) // reset write deadline
			} else {
				Log.Debug("write error", "err", "outputChannel closed")
				writeErrorChannel <- errors.New("outputChannel closed")
				break loop
			}
		case cmd, ok := <-writeControlChannel:
			if !ok {
				writeErrorChannel <- errors.New("writeControlChannel closed")
				break loop
			} else {
				switch cmd {
				case Quit:
					Log.Debug("write received Quit command")
					writeErrorChannel <- errors.New("cancelled")
					break loop
				case Ping:
					if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(3*time.Second)); err != nil {
						Log.Debug("ping error", "err", err)
						writeErrorChannel <- errors.New("cancelled")
						break loop
					}
				case UseText:
					messageType = websocket.TextMessage
				case UseBinary:
					messageType = websocket.BinaryMessage
				}
			}
		}
	}
}

func (m *Machine) keepAlive() {
	wg.Add(1)
	defer wg.Done()

	Log.Debug("keepAlive has started")

	dur, err := time.ParseDuration(m.KeepAliveInterval)
	if err != nil {
		fmt.Println("cannot parse duration")
		os.Exit(0)
	}
	timer := time.NewTimer(dur)
	timer.Stop()

loop:
	for {
		select {
		case _, ok := <-ioEventChannel:
			if ok {
				timer.Reset(dur)
			} else {
				timer.Stop()
				break loop
			}
		case <-timer.C:
			timer.Reset(dur)
			// non-blocking Ping request
			select {
			case writeControlChannel <- Ping:
			default:
			}
		}
	}
}

func (m *Machine) connect() {
	wg.Add(1)
	defer wg.Done()

	Log.Debug("connect has started")

	reconnectInterval, err := time.ParseDuration(m.ReconnectInterval)
	if err != nil {
		fmt.Println("cannot parse duration")
		os.Exit(0)
	}

	for {
		statusChannel <- Status{State: Connecting}
		dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
		conn, _, err := dialer.Dial(m.URL, m.Headers)
		if err == nil {
			conn.SetPongHandler(func(string) error { ioEventChannel <- true; return nil })
			connReturnChannel <- conn
			statusChannel <- Status{State: Connected}
			return
		} else {
			Log.Debug("connect error", "err", err)
			statusChannel <- Status{Disconnected, err}
		}

		statusChannel <- Status{State: Waiting}
		select {
		case <-time.After(reconnectInterval):
		case <-connCancelChannel:
			statusChannel <- Status{Disconnected, errors.New("cancelled")}
			return
		}
	}
}
