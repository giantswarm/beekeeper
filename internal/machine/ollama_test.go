package machine

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// labNode and otherNode are two kind nodes' addresses.
const (
	labNode   = "172.21.0.3"
	otherNode = "172.18.0.2"
)

func TestParseOllamaLoads(t *testing.T) {
	journal := `time=2026-09-30T16:34:22.164+03:00 level=INFO source=llama_server.go:434 msg="starting llama-server" cmd="/usr/lib/ollama/llama-server --model /var/lib/ollama/blobs/sha256-small --port 44597 --host 127.0.0.1"
[GIN] 2026/09/30 - 16:35:49 | 200 |         1m27s |      172.21.0.3 | POST     "/v1/chat/completions"
time=2026-09-30T16:40:51.432+03:00 level=INFO source=llama_server.go:434 msg="starting llama-server" cmd="/usr/lib/ollama/llama-server --model /var/lib/ollama/blobs/sha256-big --port 40001"
[GIN] 2026/09/30 - 16:40:52 | 200 | 512.1ms |      172.21.0.3 | POST     "/v1/chat/completions"
[GIN] 2026/09/30 - 16:41:20 | 200 | 29.436412014s |      172.21.0.2 | POST     "/api/chat"`
	got := ParseOllamaLoads(journal)
	if got["/var/lib/ollama/blobs/sha256-small"] != labNode {
		t.Errorf("small loaded by %q", got["/var/lib/ollama/blobs/sha256-small"])
	}
	// The first request after the big load started after it: the loader
	// is the one that was already running.
	if got["/var/lib/ollama/blobs/sha256-big"] != "172.21.0.2" {
		t.Errorf("big loaded by %q", got["/var/lib/ollama/blobs/sha256-big"])
	}
}

func TestParseOllamaLoadsInFlight(t *testing.T) {
	// The lab loaded the model and its request ended; the host loads it
	// again and its request still runs: no POST names that load yet.
	journal := `time=2026-09-30T17:20:00.000+02:00 level=INFO source=llama_server.go:434 msg="starting llama-server" cmd="/usr/lib/ollama/llama-server --model /var/lib/ollama/blobs/sha256-mid --port 40001"
[GIN] 2026/09/30 - 17:20:30 | 200 | 31s |      172.21.0.3 | POST     "/v1/chat/completions"
time=2026-09-30T17:36:40.000+02:00 level=INFO source=llama_server.go:434 msg="starting llama-server" cmd="/usr/lib/ollama/llama-server --model /var/lib/ollama/blobs/sha256-mid --port 40002"`
	if c, ok := ParseOllamaLoads(journal)["/var/lib/ollama/blobs/sha256-mid"]; !ok || c != "" {
		t.Errorf("an in-flight load is attributed to %q (known %v), not left to its running request", c, ok)
	}
}

func TestParseOllamaLoadsSkipsShowAndZoneChange(t *testing.T) {
	// A load from before the machine's zone changed is still open; the new
	// load runs while beekeeper asks /api/show about it. Neither the show
	// requests nor the old zone name a client for the running load.
	journal := `time=2026-09-30T18:52:28.797+03:00 level=INFO source=llama_server.go:434 msg="starting llama-server" cmd="/usr/lib/ollama/llama-server --model /var/lib/ollama/blobs/sha256-old --port 40001"
time=2026-09-30T20:31:27.585+02:00 level=INFO source=llama_server.go:436 msg="starting llama-server" cmd="/usr/lib/ollama/llama-server --model /var/lib/ollama/blobs/sha256-new --port 40002"
[GIN] 2026/09/30 - 20:31:39 | 200 |   62.594604ms |             ::1 | POST     "/api/show"
[GIN] 2026/09/30 - 20:31:45 | 200 |    1.459674ms |             ::1 | POST     "/api/show"`
	if c, ok := ParseOllamaLoads(journal)["/var/lib/ollama/blobs/sha256-new"]; !ok || c != "" {
		t.Errorf("the running load is attributed to %q (known %v)", c, ok)
	}
	journal += `
[GIN] 2026/09/30 - 20:32:30 | 200 |         63.1s |      172.21.0.3 | POST     "/api/generate"`
	if c := ParseOllamaLoads(journal)["/var/lib/ollama/blobs/sha256-new"]; c != labNode {
		t.Errorf("the finished load is attributed to %q", c)
	}
}

func TestPeersOf(t *testing.T) {
	// 11434 is 2CAA. The v4 table: a lab node's connection, one to another
	// port and a closing one. The v6 table: the dual-stack listener, a
	// v4-mapped client and two loopback clients, one of them beekeeper's
	// (its client side is inode 9).
	v4 := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:2CAA 00000000:0000 0A 00000000:00000000 00:00000000 00000000   965        0 1 1 0000000000000000 100 0 0 10 0
   1: 010015AC:2CAA 030015AC:D431 01 00000000:00000000 00:00000000 00000000   965        0 2 1 0000000000000000 20 4 30 10 -1
   2: 010015AC:1F90 040015AC:D432 01 00000000:00000000 00:00000000 00000000   965        0 3 1 0000000000000000 20 4 30 10 -1
   3: 010015AC:2CAA 050015AC:D433 06 00000000:00000000 00:00000000 00000000   965        0 4 1 0000000000000000 20 4 30 10 -1`
	v6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:2CAA 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   965        0 5 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF0000010012AC:2CAA 0000000000000000FFFF0000020012AC:C196 01 00000000:00000000 02:00000496 00000000   965        0 6 1 0000000000000000 20 4 30 10 -1
   2: 00000000000000000000000001000000:2CAA 00000000000000000000000001000000:C19E 01 00000000:00000000 02:00000496 00000000   965        0 7 1 0000000000000000 20 4 30 10 -1
   3: 00000000000000000000000001000000:2CAA 00000000000000000000000001000000:C1A0 01 00000000:00000000 02:00000496 00000000   965        0 8 1 0000000000000000 20 4 30 10 -1
   4: 00000000000000000000000001000000:C1A0 00000000000000000000000001000000:2CAA 01 00000000:00000000 02:00000496 00000000  1000        0 9 1 0000000000000000 20 4 30 10 -1`
	conns := append(parseTCPTable(v4), parseTCPTable(v6)...)
	if got := peersOf(conns, 11434, nil); !slices.Equal(got, []string{otherNode, labNode, "::1"}) {
		t.Errorf("peers = %v", got)
	}
	// Only the other loopback client (C19E, whose client side is not in
	// the table) stays once beekeeper's socket is left out.
	if got := peersOf(conns, 11434, map[string]bool{"9": true}); !slices.Equal(got, []string{otherNode, labNode, "::1"}) {
		t.Errorf("peers without beekeeper = %v", got)
	}
	noOther := slices.DeleteFunc(slices.Clone(conns), func(c tcpConn) bool { return c.inode == "7" })
	if got := peersOf(noOther, 11434, map[string]bool{"9": true}); !slices.Equal(got, []string{otherNode, labNode}) {
		t.Errorf("peers without beekeeper's only loopback client = %v", got)
	}
	if got := peersOf(conns, 8080, nil); !slices.Equal(got, []string{"172.21.0.4"}) {
		t.Errorf("peers on 8080 = %v", got)
	}
}

func TestProcessSockets(t *testing.T) {
	proc := t.TempDir()
	for pid, comm := range map[string]string{"10": "beekeeper", "11": "curl"} {
		fd := filepath.Join(proc, pid, "fd")
		if err := os.MkdirAll(fd, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(comm+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for i, target := range []string{"socket:[" + pid + "01]", "/dev/null"} {
			if err := os.Symlink(target, filepath.Join(fd, strconv.Itoa(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := processSockets(proc, "beekeeper"); len(got) != 1 || !got["1001"] {
		t.Errorf("beekeeper's sockets = %v", got)
	}
}

func TestModelBlob(t *testing.T) {
	mf := "# Modelfile generated by \"ollama show\"\nFROM /var/lib/ollama/blobs/sha256-dec5\nTEMPLATE {{ .Prompt }}\n"
	if got := modelBlob(mf); got != "/var/lib/ollama/blobs/sha256-dec5" {
		t.Errorf("blob = %q", got)
	}
	if got := modelBlob("FROM qwen3:30b\n"); got != "" {
		t.Errorf("blob of a non-blob FROM = %q", got)
	}
}

func TestReadGPUs(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "card1", "device")
	if err := os.MkdirAll(dev, 0o750); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{
		"mem_info_gtt_used":   "45097156608\n",
		"mem_info_gtt_total":  "46208954368\n",
		"mem_info_vram_used":  "8049176576\n",
		"mem_info_vram_total": "8589934592\n",
	} {
		if err := os.WriteFile(filepath.Join(dev, f), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g := readGPUs(filepath.Join(dir, "card*", "device", "mem_info_gtt_used"))
	if len(g) != 1 || g[0].Device != "card1" || g[0].GTTUsedMiB != 43008 || g[0].GTTTotalMiB != 44068 || g[0].VRAMTotMiB != 8192 {
		t.Fatalf("gpus = %+v", g)
	}
	if GTTUsedMiB(g) != 43008 {
		t.Errorf("GTTUsedMiB = %d", GTTUsedMiB(g))
	}
	if readGPUs(filepath.Join(dir, "none*", "mem_info_gtt_used")) != nil {
		t.Error("a machine without amdgpu has GPUs")
	}
}
