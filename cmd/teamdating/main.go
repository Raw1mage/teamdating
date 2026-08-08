// Command teamdating serves the travel-date voting app.
//
// Zero third-party dependencies (DD-13). The frontend is embedded into the
// binary (DD-18), so a deployment is one file: copy it anywhere and run it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	teamdating "teamdating"
	"teamdating/internal/httpapi"
	"teamdating/internal/store"
)

func main() {
	// Startup failures must be loud and fatal (tasks 1.4, 天條 11): a server
	// that silently degrades — binding a different port, running memory-only
	// because data/ is unwritable — reports success while losing user data.
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "teamdating: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr    = flag.String("addr", "127.0.0.1:8080", "listen address")
		dataDir = flag.String("data", "data", "directory for the event log")
		dev     = flag.Bool("dev", false, "serve web/ from disk instead of the embedded copy")
		base    = flag.String("base", "", "URL path prefix to strip (for a reverse proxy that does not strip it, e.g. -base=/teamdating)")
		// ★ REQUIRED BEHIND A TLS-TERMINATING PROXY, and there is no way for the
		// process to detect that it is. The magic link's scheme is otherwise
		// derived from r.TLS, which is nil for every request when the proxy
		// terminates TLS and forwards plain HTTP — so an authentication token
		// was mailed on an http:// URL to real users. X-Forwarded-Proto is NOT
		// the fix: this service trusts no forwarded header anywhere (a client
		// can send any value), so the operator declares the canonical origin
		// instead. Unset is the direct-serve/dev mode, where r.TLS is accurate.
		publicURL = flag.String("public-url", "", "canonical external origin for links in outgoing mail, scheme+host only (e.g. -public-url=https://vote.example.com). REQUIRED behind a TLS-terminating reverse proxy: without it the magic link is built from r.TLS, which such a proxy makes permanently false, and the link goes out as http://.")
		// ★ Admin is OFF unless a separate address is given, and it must never be
		// the same address as -addr. See BR_20260805: the admin enumeration used
		// to ride the public mux behind a RemoteAddr check, and a same-host
		// reverse proxy made every request in the world look like loopback, so
		// the roster was readable from the LAN. A predicate on the request cannot
		// fix that -- by the time it runs the proxy has already erased the
		// caller. A separate listener that the gateway does not route to can.
		adminAddr = flag.String("admin-addr", "", "OPTIONAL separate loopback address for the admin enumeration (e.g. 127.0.0.1:21848). Empty = admin disabled entirely. MUST NOT be published through a reverse proxy.")
	)
	flag.Parse()

	// Reject a shape that would silently produce a broken link rather than
	// accepting it and mailing it to a user (天條 11). A path here would be
	// concatenated in front of -base and produce a 404 the sender never sees.
	if *publicURL != "" {
		u, err := url.Parse(*publicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("-public-url must be scheme+host only, e.g. https://example.org (got %q)", *publicURL)
		}
	}

	assets, source, err := webFS(*dev)
	if err != nil {
		return err
	}

	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir %q: %w", *dataDir, err)
	}
	st, err := store.New(absData)
	if err != nil {
		// store.New already distinguishes "cannot create", "cannot write" and
		// "replay failed at line N" — pass the cause through unwrapped so the
		// operator sees which one it was.
		return err
	}
	defer st.Close()

	// Bind BEFORE announcing readiness. net.Listen is what actually detects a
	// busy port; http.ListenAndServe would report the same failure later and
	// after we had already printed a startup banner.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", *addr, err)
	}

	// A reverse proxy that forwards WITHOUT stripping the route prefix delivers
	// paths like /teamdating/api/health; the mux only knows /api/health. Strip
	// the prefix here so the same binary works both direct (-base="") and behind
	// such a gateway (-base=/teamdating). Empty base is the identity wrapper, so
	// direct runs are byte-for-byte unchanged.
	var handler http.Handler = httpapi.New(st, assets, source, *base, *publicURL)
	if *base != "" {
		handler = http.StripPrefix(*base, handler)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE responses (DD-15) are long-lived by design and
		// a write deadline would sever them at a fixed interval.
	}

	// Admin listener, only if explicitly asked for.
	var adminSrv *http.Server
	if *adminAddr != "" {
		// Refuse the one configuration that silently recreates the bug: admin
		// sharing the public address puts it back behind the proxy. Fail loudly
		// rather than binding something that looks fine and is not (天條 11).
		if *adminAddr == *addr {
			return fmt.Errorf("-admin-addr must differ from -addr (%s): sharing the public "+
				"listener is what exposed the admin roster through the gateway", *addr)
		}
		adminLn, err := net.Listen("tcp", *adminAddr)
		if err != nil {
			return fmt.Errorf("cannot listen on admin address %s: %w", *adminAddr, err)
		}
		adminSrv = &http.Server{
			Handler:           httpapi.NewAdmin(st),
			ReadHeaderTimeout: 10 * time.Second,
		}
		log.Printf("teamdating admin listening on http://%s  (DO NOT publish this address)", adminLn.Addr())
		go func() {
			if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("admin listener stopped: %v", err)
			}
		}()
	}

	log.Printf("teamdating listening on http://%s  (data=%s, assets=%s)",
		ln.Addr(), absData, source)

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if adminSrv != nil {
			_ = adminSrv.Shutdown(ctx)
		}
		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		// Data is already durable: every mutation fsynced its event before
		// returning success (DD-14). Shutdown flushes connections, not state.
		return nil
	}
}

// webFS picks the asset source. In -dev mode it reads web/ from disk so a CSS
// change does not need a rebuild; otherwise it uses the embedded copy, which is
// what makes the binary self-contained (DD-18).
func webFS(dev bool) (fs.FS, string, error) {
	if dev {
		if _, err := os.Stat(filepath.Join("web", "index.html")); err != nil {
			return nil, "", fmt.Errorf("-dev requires web/index.html on disk: %w", err)
		}
		return os.DirFS("web"), "disk (web/)", nil
	}
	sub, err := teamdating.Web()
	if err != nil {
		return nil, "", fmt.Errorf("embedded assets: %w", err)
	}
	// Fail loudly if the embed came out empty rather than serving 404s that
	// look like a routing problem.
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, "", fmt.Errorf("embedded assets are missing index.html: %w", err)
	}
	return sub, "embedded", nil
}
