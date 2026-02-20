package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

func main() {
	var (
		socksPort = flag.String("port", "1080", "SOCKS5 listen port")
		duration  = flag.Duration("duration", 4*time.Hour, "How long to run before exiting (e.g. 4h, 30m)")
		hostname  = flag.String("hostname", "socktails", "Tailscale node hostname")
	)
	flag.Parse()

	// Environment variables override flags.
	if v := os.Getenv("SOCKS_PORT"); v != "" {
		*socksPort = v
	}
	if v := os.Getenv("DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("invalid DURATION %q: %v", v, err)
		}
		*duration = d
	}
	if v := os.Getenv("TS_HOSTNAME"); v != "" {
		*hostname = v
	}

	authKey := os.Getenv("TAILSCALE_AUTHKEY")
	if authKey == "" {
		log.Fatal("TAILSCALE_AUTHKEY environment variable is required")
	}

	// Root context: cancelled after duration or on signal.
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("Received signal %v, shutting down", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	// Start embedded Tailscale (userspace networking, ephemeral node).
	ts := &tsnet.Server{
		Hostname:  *hostname,
		AuthKey:   authKey,
		Ephemeral: true,
		// Dir is intentionally empty: tsnet will use a temporary directory.
	}
	defer ts.Close()

	log.Println("Starting Tailscale (userspace mode)...")
	if err := ts.Start(); err != nil {
		log.Fatalf("tsnet start: %v", err)
	}

	// Wait until the node is fully online in the tailnet.
	lc, err := ts.LocalClient()
	if err != nil {
		log.Fatalf("getting local client: %v", err)
	}

	log.Println("Waiting for Tailscale to come online...")
	for {
		if ctx.Err() != nil {
			log.Fatalf("context cancelled while waiting for Tailscale: %v", ctx.Err())
		}
		st, err := lc.Status(ctx)
		if err != nil {
			log.Printf("status error (retrying): %v", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if st.BackendState == "Running" {
			ips := make([]string, 0, len(st.TailscaleIPs))
			for _, ip := range st.TailscaleIPs {
				ips = append(ips, ip.String())
			}
			log.Printf("Tailscale online — node IPs: %v", ips)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Listen for SOCKS5 connections on the Tailscale virtual interface.
	ln, err := ts.Listen("tcp", ":"+*socksPort)
	if err != nil {
		log.Fatalf("listen on :%s: %v", *socksPort, err)
	}
	defer ln.Close()

	log.Printf("SOCKS5 proxy listening on Tailscale interface port %s (duration: %s)", *socksPort, *duration)

	// Close the listener when the context expires so Accept unblocks.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed — clean shutdown.
			return
		}
		go serveSOCKS5(conn)
	}
}

// serveSOCKS5 implements a minimal SOCKS5 server (RFC 1928) that handles
// CONNECT requests and pipes data between the client and the target.
// Only "no authentication" (method 0x00) is supported.
// Outbound connections use the container's regular (non-Tailscale) network,
// routing traffic out through the Cloud Run region.
func serveSOCKS5(client net.Conn) {
	defer client.Close()

	buf := make([]byte, 256)

	// ── Greeting ──────────────────────────────────────────────────────────
	// +-----+----------+----------+
	// | VER | NMETHODS | METHODS  |
	// +-----+----------+----------+
	if _, err := io.ReadFull(client, buf[:2]); err != nil {
		return
	}
	if buf[0] != 5 {
		return // Not SOCKS5
	}
	nMethods := int(buf[1])
	if nMethods > 0 {
		if _, err := io.ReadFull(client, buf[:nMethods]); err != nil {
			return
		}
	}

	// Respond: version 5, no authentication required.
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}

	// ── Request ───────────────────────────────────────────────────────────
	// +-----+-----+-------+------+----------+----------+
	// | VER | CMD | RSV   | ATYP | DST.ADDR | DST.PORT |
	// +-----+-----+-------+------+----------+----------+
	if _, err := io.ReadFull(client, buf[:4]); err != nil {
		return
	}
	if buf[0] != 5 {
		return
	}
	if buf[1] != 1 { // Only CONNECT is supported.
		writeSOCKS5Error(client, 7) // command not supported
		return
	}

	var addr string
	switch buf[3] {
	case 1: // IPv4
		if _, err := io.ReadFull(client, buf[:4]); err != nil {
			return
		}
		addr = net.IP(buf[:4]).String()
	case 3: // Domain name
		if _, err := io.ReadFull(client, buf[:1]); err != nil {
			return
		}
		n := int(buf[0])
		if _, err := io.ReadFull(client, buf[:n]); err != nil {
			return
		}
		addr = string(buf[:n])
	case 4: // IPv6
		if _, err := io.ReadFull(client, buf[:16]); err != nil {
			return
		}
		addr = net.IP(buf[:16]).String()
	default:
		writeSOCKS5Error(client, 8) // address type not supported
		return
	}

	if _, err := io.ReadFull(client, buf[:2]); err != nil {
		return
	}
	port := int(buf[0])<<8 | int(buf[1])
	target := net.JoinHostPort(addr, fmt.Sprintf("%d", port))

	// ── Connect to target ─────────────────────────────────────────────────
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	dst, err := dialer.Dial("tcp", target)
	if err != nil {
		log.Printf("dial %s: %v", target, err)
		writeSOCKS5Error(client, 4) // host unreachable
		return
	}
	defer dst.Close()

	// ── Success reply ─────────────────────────────────────────────────────
	// VER=5, REP=0 (success), RSV=0, ATYP=1 (IPv4), BND.ADDR=0.0.0.0, BND.PORT=0
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	// ── Pipe ──────────────────────────────────────────────────────────────
	done := make(chan struct{}, 2)
	go func() { io.Copy(dst, client); done <- struct{}{} }()   //nolint:errcheck
	go func() { io.Copy(client, dst); done <- struct{}{} }()   //nolint:errcheck
	<-done
}

// writeSOCKS5Error sends a SOCKS5 error reply with the given REP code.
func writeSOCKS5Error(w io.Writer, rep byte) {
	w.Write([]byte{5, rep, 0, 1, 0, 0, 0, 0, 0, 0}) //nolint:errcheck
}
