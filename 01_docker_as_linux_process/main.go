package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

func main() {
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/eat", eatHandler)
	http.HandleFunc("/burn", burnHandler)
	http.HandleFunc("/settime", settimeHandler)

	addr := ":8080"
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func settimeHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	tv := syscall.NsecToTimeval(now.UnixNano())

	err := syscall.Settimeofday(&tv)
	if err != nil {
		http.Error(w, fmt.Sprintf("settimeofday failed: %v\n", err), http.StatusForbidden)
		return
	}
	fmt.Fprintf(w, "clock reset to %s\n", now.Format(time.RFC3339))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "ok!\n")
}

// eatHandler allocates ?mb=N megabytes and holds onto them for the
// lifetime of the request (so you can inspect RSS / trigger OOM while it's alive).
func eatHandler(w http.ResponseWriter, r *http.Request) {
	mbStr := r.URL.Query().Get("mb")
	mb, err := strconv.Atoi(mbStr)
	if err != nil || mb <= 0 {
		http.Error(w, "usage: /eat?mb=N (N > 0)", http.StatusBadRequest)
		return
	}

	buf := make([]byte, mb*1024*1024)
	// touch every page so it's actually resident, not just reserved virtual memory
	for i := range buf {
		buf[i] = 1
	}

	fmt.Fprintf(w, "allocated and touched %d MB\n", mb)
	// buf goes out of scope after the handler returns and can be GC'd —
	// good enough to observe RSS spike / OOM during the request itself.
}

// burnHandler pegs one CPU core in a tight loop until the client disconnects.
func burnHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "burning...")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	ctx := r.Context()
	x := 0.0
	for {
		select {
		case <-ctx.Done():
			return
		default:
			x += 1.0000001
			if x > 1e18 {
				x = 0
			}
		}
	}
}
