package main

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestEvalExternalBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds qh")
	}
	bin := os.Getenv("QH_TEST_BIN") // to check an older build
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "qh")
		if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	t.Setenv("QH_HOME", t.TempDir())
	fm := &fakeModel{}
	ts := httptest.NewServer(fm)
	defer ts.Close()
	e, err := startExtEval(bin, ts.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	home := e.home
	evalExt = e
	defer func() { evalExt = nil }()
	a := &Agent{}
	task := EvalTask{Name: "echo", Prompt: "list files", Check: "test -f a.txt", Files: map[string]string{"a.txt": "x\n"}}
	r := a.runTask(context.Background(), task, 1, time.Minute, io.Discard)
	e.close()
	// fakeModel: a bash call, then the answer; usage 100 prompt, 90 cached, 7 out each
	if !r.Pass || r.Steps != 2 || r.Requests != 2 || r.Prompt != 100 || r.Fresh != 20 || r.Output != 14 || r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("the temporary QH_HOME is left")
	}
}
