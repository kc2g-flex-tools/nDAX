package main

import (
	"fmt"
	"net"
	"os"
)

type PulseCLI struct {
	conn net.Conn
}

func NewPulseCLI() (*PulseCLI, error) {
	conn, err := net.Dial("unix", fmt.Sprint("/run/user/", os.Getuid(), "/pulse/cli"))

	if err != nil {
		return nil, fmt.Errorf("connect to pulse CLI socket: %w", err)
	}

	return &PulseCLI{
		conn: conn,
	}, nil
}

func (c *PulseCLI) Send(command string) error {
	_, err := c.conn.Write([]byte(command + "\r\n"))
	return err
}

func (c *PulseCLI) Close() {
	c.conn.Close()
}
