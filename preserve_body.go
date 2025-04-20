package main

import (
	"bytes"
	"io"
)

type PreserveBodyReader struct {
}

type PreserveBodyReaderMain struct {
	parent io.ReadCloser
	buf    *bytes.Buffer
}

type PreserveBodyReaderCopy struct {
	parent io.ReadCloser
	buf    *bytes.Buffer
}

func NewPreserveBodyReader(parent io.ReadCloser) (io.ReadCloser, io.ReadCloser) {
	if parent == nil {
		return nil, nil
	}
	buf := new(bytes.Buffer)
	return PreserveBodyReaderMain{parent: parent, buf: buf}, PreserveBodyReaderCopy{parent: parent, buf: buf}
}

func (r PreserveBodyReaderMain) Read(p []byte) (n int, err error) {
	if r.parent == nil {
		return 0, io.EOF
	}
	n, err = r.parent.Read(p)
	r.buf.Write(p[:n])
	return
}

func (r PreserveBodyReaderMain) Close() error {
	return nil
}

func (r PreserveBodyReaderCopy) Read(p []byte) (n int, err error) {
	n, err = r.buf.Read(p)
	if err == io.EOF && r.parent != nil {
		n, err = r.parent.Read(p)
	}
	return
}

func (r PreserveBodyReaderCopy) Close() error {
	if r.parent == nil {
		return nil
	}
	return r.parent.Close()
}
