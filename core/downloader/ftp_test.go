package downloader

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gluestick-sh/core/store"
)

// tele2FTP1MB is a public FTP speed-test file (1 MiB).
const tele2FTP1MB = "ftp://speedtest.tele2.net/1MB.zip"

func TestIsFTPURL(t *testing.T) {
	if !isFTPURL("ftp://download.tuxfamily.org/opendungeons/0.7/OpenDungeons-0.7.1-Win32-MSVS2013.zip") {
		t.Fatal("expected ftp URL")
	}
	if isFTPURL("https://example.com/file.zip") {
		t.Fatal("https should not be ftp")
	}
}

func TestDownloaderFTPLocal(t *testing.T) {
	content := []byte("glue ftp local test payload")
	addr, cleanup := startLocalTestFTPServer(t, "sample.bin", content)
	defer cleanup()

	tmpDir, err := os.MkdirTemp("", "downloader-ftp-local-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	d := NewDownloader(store)
	results := d.DownloadAll(context.Background(), []Task{
		{URL: "ftp://" + addr + "/sample.bin", Filename: "sample.bin"},
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Error != nil {
		t.Fatalf("local ftp download failed: %v", results[0].Error)
	}
	if results[0].Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", results[0].Size, len(content))
	}
	if results[0].Hash == "" {
		t.Fatal("expected content hash")
	}
}

// TestDownloaderFTPPublic downloads from Tele2's public FTP speed-test host.
// Skips when FTP is blocked (common behind transparent proxies / VPN fake-IP).
func TestDownloaderFTPPublic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping public FTP integration test in short mode")
	}

	tmpDir, err := os.MkdirTemp("", "downloader-ftp-public-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	d := NewDownloader(store)
	results := d.DownloadAll(context.Background(), []Task{
		{URL: tele2FTP1MB, Filename: "1MB.zip"},
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	result := results[0]
	if result.Error != nil {
		t.Skipf("public FTP unreachable (FTP may be blocked on this network): %v", result.Error)
	}
	if result.Hash == "" {
		t.Fatal("expected content hash")
	}
	const wantBytes = 1 << 20
	if result.Size != wantBytes {
		t.Fatalf("size = %d, want %d", result.Size, wantBytes)
	}
}

// startLocalTestFTPServer serves a single anonymous file for downloader tests.
func startLocalTestFTPServer(t *testing.T, name string, content []byte) (addr string, cleanup func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
					continue
				}
			}
			go serveLocalFTPConn(conn, name, content)
		}
	}()

	cleanup = func() {
		close(stop)
		_ = ln.Close()
		wg.Wait()
	}
	return ln.Addr().String(), cleanup
}

func serveLocalFTPConn(ctrl net.Conn, fileName string, content []byte) {
	defer ctrl.Close()
	reader := bufio.NewReader(ctrl)
	writeLine := func(format string, args ...any) {
		_, _ = fmt.Fprintf(ctrl, format+"\r\n", args...)
	}

	var pendingData net.Listener

	writeLine("220 glue test FTP ready")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		cmd := strings.ToUpper(parts[0])
		switch cmd {
		case "USER":
			writeLine("331 password please")
		case "PASS":
			writeLine("230 logged in")
		case "TYPE":
			writeLine("200 type set")
		case "SYST":
			writeLine("215 UNIX Type: L8")
		case "FEAT":
			writeLine("211-Features")
			writeLine(" EPSV")
			writeLine(" PASV")
			writeLine("211 End")
		case "OPTS":
			writeLine("200 ok")
		case "PWD", "CWD":
			writeLine("250 ok")
		case "SIZE":
			writeLine("213 %d", len(content))
		case "EPSV":
			if pendingData != nil {
				_ = pendingData.Close()
				pendingData = nil
			}
			dataLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				writeLine("425 cannot open data connection")
				continue
			}
			pendingData = dataLn
			port := dataLn.Addr().(*net.TCPAddr).Port
			writeLine("229 Entering Extended Passive Mode (|||%d|)", port)
		case "PASV":
			if pendingData != nil {
				_ = pendingData.Close()
				pendingData = nil
			}
			dataLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				writeLine("425 cannot open data connection")
				continue
			}
			pendingData = dataLn
			tcp := dataLn.Addr().(*net.TCPAddr)
			p1 := tcp.Port / 256
			p2 := tcp.Port % 256
			writeLine("227 Entering Passive Mode (127,0,0,1,%d,%d)", p1, p2)
		case "RETR":
			if pendingData == nil {
				writeLine("425 use EPSV or PASV first")
				continue
			}
			writeLine("150 sending %s", fileName)
			dataConn, err := pendingData.Accept()
			_ = pendingData.Close()
			pendingData = nil
			if err != nil {
				writeLine("426 connection failed")
				continue
			}
			_, _ = dataConn.Write(content)
			_ = dataConn.Close()
			writeLine("226 transfer complete")
		case "QUIT":
			if pendingData != nil {
				_ = pendingData.Close()
			}
			writeLine("221 bye")
			return
		case "NOOP":
			writeLine("200 ok")
		case "AUTH", "PBSZ", "PROT":
			writeLine("502 not implemented")
		default:
			writeLine("500 unknown command %s", strconv.Quote(cmd))
		}
	}
}
