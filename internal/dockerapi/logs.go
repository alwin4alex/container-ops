package dockerapi

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"
)

// LogLine is one parsed line of `docker service logs` output.
type LogLine struct {
	Time   time.Time
	TaskID string // full task id from the details prefix (empty for TTY services)
	NodeID string
	Stream string // stdout | stderr
	Msg    string
}

// ReadLogs reads a service log stream and calls fn for every line until the
// stream ends or fn returns false. It handles both the multiplexed format
// (8-byte frame headers) and raw TTY output.
func ReadLogs(r io.Reader, fn func(LogLine) bool) error {
	br := bufio.NewReaderSize(r, 64*1024)
	head, err := br.Peek(8)
	if err != nil {
		if errors.Is(err, io.EOF) && len(head) == 0 {
			return nil
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
	}
	if len(head) == 8 && (head[0] == 1 || head[0] == 2) && head[1] == 0 && head[2] == 0 && head[3] == 0 {
		return readMux(br, fn)
	}
	return readRaw(br, "stdout", fn)
}

func readMux(r *bufio.Reader, fn func(LogLine) bool) error {
	hdr := make([]byte, 8)
	var partial = map[byte]string{}
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(hdr[4:8])
		if size > 1<<20 {
			return errors.New("log frame too large")
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil
		}
		stream := "stdout"
		if hdr[0] == 2 {
			stream = "stderr"
		}
		text := partial[hdr[0]] + string(buf)
		lines := strings.Split(text, "\n")
		partial[hdr[0]] = lines[len(lines)-1]
		for _, l := range lines[:len(lines)-1] {
			if !fn(parseLine(l, stream)) {
				return nil
			}
		}
	}
}

func readRaw(r *bufio.Reader, stream string, fn func(LogLine) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if !fn(parseLine(sc.Text(), stream)) {
			return nil
		}
	}
	return sc.Err()
}

// parseLine splits "<timestamp> <details> <message>". With details=1, the
// service logs endpoint prefixes task info as key=value pairs, e.g.
// "com.docker.swarm.node.id=..,com.docker.swarm.service.id=..,com.docker.swarm.task.id=..".
func parseLine(l, stream string) LogLine {
	out := LogLine{Stream: stream}
	l = strings.TrimRight(l, "\r")
	if ts, rest, ok := strings.Cut(l, " "); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			out.Time = t
			l = rest
		}
	}
	if first, rest, ok := strings.Cut(l, " "); ok && strings.Contains(first, "com.docker.swarm.") {
		out.TaskID, out.NodeID = taskFromDetails(first)
		l = rest
	}
	out.Msg = l
	return out
}

func taskFromDetails(d string) (task, node string) {
	for _, kv := range strings.Split(d, ",") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "com.docker.swarm.task.id":
			task = v
		case "com.docker.swarm.node.id":
			node = v
		}
	}
	return task, node
}
