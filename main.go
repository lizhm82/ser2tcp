package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"

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

	var outBuf bytes.Buffer
	buf := make([]byte, 1024)

	for {
		n, err := ser.Read(buf)
		if err != nil {
			log.Println(err)
			break
		}

		// fmt.Println(hex.Dump(buf[:n]))

		for _, v := range buf[:n] {
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
				outBuf.WriteByte(v)
				evtlenCalc++
				csumCalc = csumCalc ^ v

				if evtlenCalc == evtlen {
					evtlenCalc = 0 // reset

					// fmt.Println(hex.Dump(outBuf.Bytes()))

					// check check sum
					if csumCalc != csum {
						// discard latest event, for its broken
						log.Println("Checksum incorrected")

						outBuf.Truncate(outBuf.Len() - evtlen)
					}

					state = sLookingMark0
				}
			}
		}

		// forwarding to TCP
		avail := outBuf.Len()
		if avail > 0 {
			// excluding uncompleted event
			conn.Write(outBuf.Next(avail - evtlenCalc))

			// now uncompleted event still in @outBuf
		}

	}
}

func main() {
	fmt.Println("Ser2Tcp")

	// open serial port
	serconf := &serial.Config{Name: "COM3", Baud: 1500000}
	ser, err := serial.OpenPort(serconf)
	if err != nil {
		log.Fatal(err)
	}
	defer ser.Close()

	// listening on TCP port
	ln, err := net.Listen("tcp", ":7788")
	if err != nil {
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
