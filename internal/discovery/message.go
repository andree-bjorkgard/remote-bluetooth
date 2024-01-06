package discovery

import (
	"log"
	"strconv"
)

const portByteLength = 5

type message []byte

func (m message) getMessageBytes() []byte {
	return m[:len(m)-(portByteLength+1)]
}

func (m message) getPortBytes() []byte {
	return m[len(m)-(portByteLength):]
}

func (m message) getPort() int {
	port, err := strconv.Atoi(string(m.getPortBytes()))
	if err != nil {
		log.Fatalf("message.getPort: %s", err)
	}

	return port
}

func (m *message) setPort(port int) {
	*m = append(*m, []byte(":")...)
	*m = append(*m, padLeftWithZeros([]byte(strconv.Itoa(port)), portByteLength)...)
}
