package discovery

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseDarwinProcArgs(t *testing.T) {
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, 3)
	raw = append(raw, []byte("/opt/cli-proxy-api\x00\x00\x00")...)
	raw = append(raw, []byte("/opt/cli-proxy-api\x00-config\x00/Users/me/My Config/config.yaml\x00IGNORED=value\x00")...)

	executable, argv, err := parseDarwinProcArgs(raw)
	if err != nil {
		t.Fatalf("parseDarwinProcArgs returned error: %v", err)
	}
	if executable != filepath.Clean("/opt/cli-proxy-api") {
		t.Fatalf("executable = %q", executable)
	}
	wantArgv := []string{"/opt/cli-proxy-api", "-config", "/Users/me/My Config/config.yaml"}
	if !reflect.DeepEqual(argv, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", argv, wantArgv)
	}

	evidence := normalizeProcessEvidence(ProcessEvidence{PID: 42, Executable: executable, Argv: argv})
	if evidence.ConfigPath != filepath.Clean("/Users/me/My Config/config.yaml") {
		t.Fatalf("config path = %q", evidence.ConfigPath)
	}
}

func TestParseDarwinProcArgsRejectsMalformedAndBoundedArgc(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want error
	}{
		{name: "missing header", raw: []byte{1, 2, 3}, want: errMalformedProcessArgs},
		{name: "unterminated executable", raw: append([]byte{0, 0, 0, 0}, []byte("/opt/cli-proxy-api")...), want: errMalformedProcessArgs},
		{name: "missing argv", raw: append([]byte{1, 0, 0, 0}, []byte("/opt/cli-proxy-api\x00\x00")...), want: errMalformedProcessArgs},
	}
	tooMany := make([]byte, 4)
	binary.LittleEndian.PutUint32(tooMany, maxProcessArgCount+1)
	tests = append(tests, struct {
		name string
		raw  []byte
		want error
	}{name: "too many arguments", raw: tooMany, want: errProcessArgsTooLarge})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := parseDarwinProcArgs(test.raw)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}
