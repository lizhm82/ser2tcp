package main

import (
	"bytes"
	"container/list"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"

	"github.com/tarm/serial"
)

type parseState int

const (
	sLookingMark0 parseState = iota
	sLookingMark1
	sLookingEventLen
	sLookingChkSum
	sLookingEvent
)

var (
	serialDevice  = flag.String("s", "COM3", "Serial device name")
	serialBaud    = flag.Int("b", 1500000, "Serial baud rate in bps")
	tcpListenPort = flag.Int("l", 7788, "TCP Listen port number")
	showHelp      = flag.Bool("h", false, "Show this help")
)

/*
 * TCP --> Serial
 *
 */
func tcp2ser(conn io.Reader, ser io.Writer) {
	buf := make([]byte, 128)

	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Println(err)
			break
		}

		fmt.Println(buf[:n])

		n, err = ser.Write(buf[:n])
		if err != nil {
			log.Println(err)
			break
		}
	}
}

type eventBufPool struct {
	bufNumLimit int
	bufNumAlloc int
	freeList    *list.List
	freeListMux sync.Mutex

	usedList    *list.List
	usedListMux sync.Mutex
}

func eventBufPoolCreate(numLimit int) *eventBufPool {
	var pool eventBufPool

	pool.freeList = list.New()
	pool.usedList = list.New()

	pool.bufNumLimit = numLimit
	pool.bufNumAlloc = 1

	// prealloc
	for i := 0; i < pool.bufNumAlloc; i++ {
		buf := new(bytes.Buffer)
		pool.freeList.PushBack(buf)
	}

	return &pool
}

func eventBufPoolGetFromFree(pool *eventBufPool) *bytes.Buffer {
	pool.freeListMux.Lock()
	defer pool.freeListMux.Unlock()

	e := pool.freeList.Front()
	if e != nil {
		return pool.freeList.Remove(e).(*bytes.Buffer)
	}

	// There is no free eventBuf in list, alloc now
	if pool.bufNumAlloc < pool.bufNumLimit {
		buf := new(bytes.Buffer)
		pool.bufNumAlloc++
		log.Println("alloc ", pool.bufNumAlloc)
		return buf
	}

	return nil
}

func eventBufPoolGetFromUsed(pool *eventBufPool) *bytes.Buffer {
	pool.usedListMux.Lock()
	defer pool.usedListMux.Unlock()

	e := pool.usedList.Front()
	if e != nil {
		return pool.usedList.Remove(e).(*bytes.Buffer)
	}

	return nil
}

func eventBufPoolPutToFree(pool *eventBufPool, buf *bytes.Buffer) {
	pool.freeListMux.Lock()
	defer pool.freeListMux.Unlock()

	pool.freeList.PushBack(buf)
}

func eventBufPoolPutToUsed(pool *eventBufPool, buf *bytes.Buffer) {
	pool.usedListMux.Lock()
	defer pool.usedListMux.Unlock()

	pool.usedList.PushBack(buf)
}

/*
 * Serial --> TCP
 *
 * serial coming data format : '0xAD 0xDE len csum <len bytes EVENT>'
 * forwarding to tcp client data, just keep the '<len bytes EVENT>'
 */
func ser2tcp(ser io.Reader, conn io.Writer) {
	state := sLookingMark0
	var csum, csumCalc byte
	var evtlen, evtlenCalc int

	buf := make([]byte, 1024)

	// communication flag
	readyChan := make(chan int)
	defer close(readyChan)

	pool := eventBufPoolCreate(10)

	go forwardingToTCP(conn, readyChan, pool)

	eventBuf := eventBufPoolGetFromFree(pool)

	for {
		n, err := ser.Read(buf)
		if err != nil {
			log.Println(err)
			break
		}
		// fmt.Println("Serail Received\n", hex.Dump(buf[:n]))

		// parse event then put to eventBuf
		for index := 0; index < n; index++ { // for _, v := range buf[:n] {
			v := buf[index]
			switch state {
			case sLookingMark0:
				if v == 0xAD {
					state = sLookingMark1
				} else {
					log.Printf("LookingMark0 : Drop %x", v)
				}
			case sLookingMark1:
				if v == 0xDE {
					state = sLookingEventLen
				} else {
					log.Printf("LookingMark1 : Drop %x", v)
					state = sLookingMark0
				}
			case sLookingEventLen:
				evtlen = int(v)
				state = sLookingChkSum
			case sLookingChkSum:
				csum = v
				state = sLookingEvent
				evtlenCalc = 0
				csumCalc = 0
			case sLookingEvent:
				eventBuf.WriteByte(v)
				evtlenCalc++
				csumCalc = csumCalc ^ v

				if evtlenCalc == evtlen {
					evtlenCalc = 0 // reset

					// check check sum
					if csumCalc != csum {
						// discard latest event, for its broken
						log.Println("Checksum incorrected")

						eventBuf.Truncate(eventBuf.Len() - evtlen)

						// roll back to search new event
						index -= evtlen
						if index < 0 { // happens when a event be splited in two buffer
							index = -1
						}
					}

					// fmt.Println("Current Events\n", hex.Dump(eventBuf.Bytes()))

					state = sLookingMark0
				}
			}
		}

		// forwarding to TCP
		avail := eventBuf.Len()
		if avail > 0 {
			newBuf := eventBufPoolGetFromFree(pool)
			if newBuf == nil {
				log.Fatal("no more eventBuf in pool")
			}

			if evtlenCalc > 0 {
				// copy the incompleted event to the newBuf
				epart := eventBuf.Bytes()[(avail - evtlenCalc):]
				b := make([]byte, len(epart))
				copy(b, epart)
				newBuf.Write(b)

				// forwarding the eventBuf exclude the incompleted event
				eventBuf.Truncate(avail - evtlenCalc)
			}

			eventBufPoolPutToUsed(pool, eventBuf)

			// notify event buffer ready
			readyChan <- 1

			eventBuf = newBuf
		}

	}
}

func forwardingToTCP(tcp io.Writer, readyChan chan int, pool *eventBufPool) {
	var rc <-chan int = readyChan // rc is receive-only channel

	for {
		_, ok := <-rc
		if !ok { // channel closed
			break
		}

		eventBuf := eventBufPoolGetFromUsed(pool)
		if eventBuf == nil {
			continue
		}

		eventBuf.WriteTo(tcp)

		eventBufPoolPutToFree(pool, eventBuf)
	}
}

func main() {
	flag.Parse()

	if *showHelp {
		showUsage()
	}

	fmt.Println("Ser2Tcp")

	// open serial port
	serconf := &serial.Config{Name: *serialDevice, Baud: *serialBaud}
	ser, err := serial.OpenPort(serconf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open %s\n", *serialDevice)
		log.Fatal(err)
	}
	defer ser.Close()

	// listening on TCP port
	port := fmt.Sprintf(":%d", *tcpListenPort)
	fmt.Println(port)
	ln, err := net.Listen("tcp", port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot listen on %s\n", port)
		log.Fatal(err)
	}
	defer ln.Close()

	for {
		// wait for a connection
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}

		log.Println("Tcp client connected: ", conn.RemoteAddr())

		go tcp2ser(conn, ser)
		go ser2tcp(ser, conn)
	}

}

func showUsage() {
	fmt.Fprintf(os.Stderr, "Usage: ser2tcp -s <serial device> -b <serial baud> -p <tcp port listen>\n")
	flag.PrintDefaults()
	os.Exit(2)
}
