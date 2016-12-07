package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"time"

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

	lastForwadingTime := time.Now()

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
					if csumCalc == csum {
						// buffer enough events, then forward to tcp
						if outBuf.Len() >= 1024 {
							log.Println("Forwarding..")
							fmt.Println(hex.Dump(outBuf.Bytes()))

							outBuf.WriteTo(conn)
							lastForwadingTime = time.Now()
						}
					} else {
						// discard latest event, for its broken
						log.Println("Checksum incorrected")

						outBuf.Truncate(outBuf.Len() - evtlen)
					}

					state = sLookingMark0
				}
			}
		}

		// the duration since last forwarding data longer, force forwarding.
		if time.Now().After(lastForwadingTime.Add(time.Second * 2)) {
			avail := outBuf.Len()
			if avail > 0 {
				conn.Write(outBuf.Next(avail - evtlenCalc)) // excluding uncompleted event

				// now uncompleted event still in @outBuf
				lastForwadingTime = time.Now()
			}
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

		// go forward(conn, ser)
		// go forward(ser, conn)

		go tcp2ser(conn, ser)
		go ser2tcp(ser, conn)
	}

}

func forward(dest io.Writer, src io.Reader) {
	var name [2]string
	arr := []interface{}{dest, src}
	for i, dir := range arr {
		switch dir.(type) {
		case net.Conn:
			name[i] = "Tcp"
		case *serial.Port:
			name[i] = "Serial"
		}
	}

	log.Println("Start forwarding : ", name[0], "<-- ", name[1])

	buf := make([]byte, 128)

	for {
		n, err := src.Read(buf)
		if err != nil {
			log.Println(err)
			break
		}
		log.Printf("%s read : %q\n", name[1], buf[:n])
		n, err = dest.Write(buf[:n])
		if err != nil {
			log.Println(err)
			break
		}
		// log.Println(n)
	}

}
