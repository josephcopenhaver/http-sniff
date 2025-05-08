package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func rootContext(logger *slog.Logger) (context.Context, func()) {

	ctx, cancel := context.WithCancel(context.Background())

	procDone := make(chan os.Signal, 1)

	signal.Notify(procDone, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer cancel()

		const evtName = "shutdown requested"
		const typeName = "signaler"

		done := ctx.Done()

		select {
		case <-procDone:
			if logger == nil {
				return
			}

			logger.LogAttrs(ctx, slog.LevelWarn,
				evtName,
				slog.String(typeName, "process"),
			)
		case <-done:
			if logger == nil {
				return
			}

			logger.LogAttrs(ctx, slog.LevelWarn,
				evtName,
				slog.String(typeName, "context"),
				slog.Any("error", ctx.Err()),
				slog.Any("cause", context.Cause(ctx)),
			)
		}
	}()

	return ctx, cancel
}

func newLogger() (*slog.Logger, error) {
	level := slog.LevelInfo

	if s, ok := os.LookupEnv("LOG_LEVEL"); ok {
		s := strings.TrimSpace(s)
		if s == "" {
			return nil, errors.New("LOG_LEVEL env var was empty")
		}

		var v slog.Level
		if err := v.UnmarshalText([]byte(s)); err != nil {
			return nil, fmt.Errorf("failed to parse LOG_LEVEL: %w", err)
		}

		level = v
	}

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: level < slog.LevelDebug,
		Level:     level,
	})), nil
}

func main() {
	logger, err := newLogger()
	if err != nil {
		panic(err)
	}

	slog.SetDefault(logger)

	var ctx context.Context
	{
		v, cancel := rootContext(logger)
		defer cancel()

		ctx = v
	}

	var cmd string
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var p func(context.Context, *slog.Logger) error
	switch cmd {
	case "serve":
		p = handleServe
	case "req":
		p = handleReq
	default:
		panic("expecting one argument, which should be one of: serve, req")
	}

	if err := p(ctx, logger); err != nil {
		panic(err)
	}
}

func getAddr(envHost, envPort string, allowZeroPort bool) (string, error) {
	var host = "127.0.0.1"
	var port = 8080

	if v, ok := os.LookupEnv(envHost); ok {
		v := strings.TrimSpace(v)

		if strings.ContainsRune(v, ':') {
			return "", errors.New(envHost + " has an invalid value")
		}

		host = v
	}

	if v, ok := os.LookupEnv(envPort); ok {
		v := strings.TrimSpace(v)

		if v == "" {
			return "", errors.New(envPort + " was empty")
		}

		n, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf(envPort+" not an integer: %w", err)
		}

		if strconv.Itoa(n) != v {
			return "", errors.New(envPort + " not a valid integer")
		}

		if allowZeroPort {
			if n < 0 || n > 65_535 {
				return "", errors.New(envPort + " not within range [0, 65535]")
			}
		} else {
			if n < 1 || n > 65_535 {
				return "", errors.New(envPort + " not within range [1, 65535]")
			}
		}

		port = n
	}

	result := net.JoinHostPort(host, strconv.Itoa(port))

	if checkHost, _, err := net.SplitHostPort(result); err != nil {
		return "", fmt.Errorf(envHost+" is malformed: %w", err)
	} else if checkHost != host {
		return "", errors.New(envHost + " format is invalid")
	}

	return result, nil
}

func jsonDataToLine(b []byte, end []byte) []byte {
	var result []byte
	var sig [32]byte
	sep := []byte{','}

	// -1 to remove terminal newline character
	v := make([]byte, ((len(sig[:])+2)*4/3)+len(sep)+((len(b)+2-1)*4/3)+len(end))
	result = v

	sig = sha256.Sum256(b)

	encode := base64.RawURLEncoding.Encode

	encode(v, sig[:])
	v = v[((len(sig) + 2) * 4 / 3):]

	copy(v, sep)
	v = v[len(sep):]

	// -1 removes terminal newline character
	encode(v, b[:len(b)-1])
	// -1 to account for removed terminal newline character
	v = v[((len(b) + 2 - 1) * 4 / 3):]

	copy(v, end)

	return result
}

type reqObj struct {
	Proto      string
	Host       string
	Method     string
	RequestURI string
	Header     http.Header
	Body       string
	ReadErr    bool `json:",omitempty"`
}

func handleServe(ctx context.Context, logger *slog.Logger) error {
	const shutdownTimeout = 24 * time.Second

	ctx, stopServer := context.WithCancel(ctx)
	defer stopServer()

	defer logger.LogAttrs(ctx, slog.LevelWarn,
		"done",
	)

	addr, err := getAddr("LISTEN_HOST", "LISTEN_PORT", true)
	if err != nil {
		return fmt.Errorf("failed to determine ADDR: %w", err)
	}

	logger.LogAttrs(ctx, slog.LevelInfo,
		"listener starting",
		slog.String("addr", addr),
	)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	closeListener := sync.OnceFunc(func() {
		defer listener.Close()

		logger.LogAttrs(ctx, slog.LevelWarn,
			"closing listener",
		)
	})
	defer closeListener()

	logger.LogAttrs(ctx, slog.LevelInfo,
		"listening",
		slog.String("addr", listener.Addr().String()),
	)

	var stdoutM sync.Mutex

	var stdoutWG sync.WaitGroup
	defer stdoutWG.Wait()
	defer logger.LogAttrs(ctx, slog.LevelWarn, "waiting on printing to stop")

	var connWG sync.WaitGroup
	defer connWG.Wait()
	defer logger.LogAttrs(ctx, slog.LevelWarn, "waiting on connections to drain")

	srv := http.Server{
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		ConnState: func(c net.Conn, cs http.ConnState) {
			switch cs {
			case http.StateNew:
				connWG.Add(1)
			case http.StateClosed:
				connWG.Done()
			case http.StateHijacked:
				panic("a connection was hijacked unexpectedly")
				// if there is a new feature call connWG.Done()
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqData := reqObj{
				Proto:      r.Proto,
				Host:       r.Host,
				Method:     r.Method,
				RequestURI: r.URL.RequestURI(),
				Header:     r.Header.Clone(),
			}

			var body bytes.Buffer
			if _, err := io.Copy(&body, r.Body); err != nil {
				reqData.Body = body.String()
				reqData.ReadErr = true
				stdoutWG.Add(1)
				go func() {
					defer stdoutWG.Done()

					var buf bytes.Buffer
					enc := json.NewEncoder(&buf)
					enc.SetEscapeHTML(false)

					if err := enc.Encode(reqData); err != nil {
						logger.LogAttrs(ctx, slog.LevelError,
							"failed to encode req data to json for failed copy context",
							slog.String("error", err.Error()),
						)
						return
					}

					stdoutData := jsonDataToLine(buf.Bytes(), []byte{'\n', '\n'})

					func() {
						stdoutM.Lock()
						defer stdoutM.Unlock()

						if _, err := os.Stdout.Write(stdoutData); err != nil {
							logger.LogAttrs(ctx, slog.LevelError,
								"failed to write data to stdout for failed copy context",
								slog.String("error", err.Error()),
							)

							stopServer()
						}
					}()
				}()

				w.WriteHeader(http.StatusBadRequest)
				return
			}

			reqData.Body = body.String()

			stdoutWG.Add(1)
			go func() {
				defer stdoutWG.Done()

				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)

				if err := enc.Encode(reqData); err != nil {
					logger.LogAttrs(ctx, slog.LevelError,
						"failed to encode req data to json",
						slog.String("error", err.Error()),
					)
					return
				}

				stdoutData := jsonDataToLine(buf.Bytes(), []byte{'\n', '\n'})

				func() {
					stdoutM.Lock()
					defer stdoutM.Unlock()

					if _, err := os.Stdout.Write(stdoutData); err != nil {
						logger.LogAttrs(ctx, slog.LevelError,
							"failed to write data to stdout",
							slog.String("error", err.Error()),
						)

						stopServer()
					}
				}()
			}()

			w.Header().Set("Content-Type", "application/json")

			w.WriteHeader(http.StatusOK)

			w.Write([]byte("{}\n"))
		}),
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	defer logger.LogAttrs(ctx, slog.LevelWarn,
		"waiting for server goroutines to finish",
	)

	wg.Add(2)
	errChan := make(chan error, 2)

	go func() {
		defer wg.Done()
		defer closeListener() // ideally we'd do draining or load shedding, but this is a toy example

		err := srv.Serve(listener)
		if err != nil && ctx.Done() != nil && errors.Is(err, http.ErrServerClosed) {
			err = nil
		}

		errChan <- err
	}()

	go func() {
		defer wg.Done()
		defer closeListener() // ideally we'd do draining or load shedding, but this is a toy example

		<-ctx.Done()

		ctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), time.Now().Add(shutdownTimeout))
		defer cancel()

		errChan <- srv.Shutdown(ctx)
	}()

	logger.LogAttrs(ctx, slog.LevelInfo,
		"ready",
	)

	func() {
		stdoutM.Lock()
		defer stdoutM.Unlock()

		if _, err := os.Stdout.Write([]byte{'\n'}); err != nil {
			logger.LogAttrs(ctx, slog.LevelError,
				"failed to write passive first newline to stdout",
				slog.String("error", err.Error()),
			)

			stopServer()
		}
	}()

	if err := <-errChan; err != nil {
		return fmt.Errorf("server failed to shutdown gracefully: %w", err)
	}

	return nil
}

type respObj struct {
	Proto      string
	Status     string
	StatusCode int
	Date       int
	Header     http.Header
	Body       string
}

func handleReq(ctx context.Context, logger *slog.Logger) error {
	const reqTimeout = 5 * time.Second

	addr, err := getAddr("HOST", "PORT", false)
	if err != nil {
		return fmt.Errorf("failed to determine ADDR: %w", err)
	}

	var hostname string
	if host, portStr, _ := net.SplitHostPort(addr); portStr == "80" {
		hostname = host
		addr = host
	} else {
		hostname = host
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", strings.NewReader("{}\n"))
	if err != nil {
		return fmt.Errorf("failed to construct request: %w", err)
	}

	req.Header.Set("Host", hostname)
	req.Header.Set("User-Agent", "httpsniff/0.1")

	c := http.Client{Timeout: reqTimeout}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	var body bytes.Buffer
	if _, err := io.Copy(&body, resp.Body); err != nil {
		return fmt.Errorf("failed to read entire response body: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	h := resp.Header.Clone()
	var dateVal int
	if v, ok := h["Date"]; ok {
		dateVal = len(v)
	} else {
		dateVal = -1
	}
	h.Del("Date")

	respData := respObj{
		Proto:      resp.Proto,
		Status:     resp.Status,
		StatusCode: resp.StatusCode,
		Date:       dateVal,
		Header:     h,
		Body:       body.String(),
	}

	if err := enc.Encode(respData); err != nil {
		logger.LogAttrs(ctx, slog.LevelError,
			"failed to encode resp data to json",
			slog.String("error", err.Error()),
		)
	}

	stdoutData := jsonDataToLine(buf.Bytes(), []byte{'\n'})

	if _, err := os.Stdout.Write(stdoutData); err != nil {
		return fmt.Errorf("failed to write data to stdout: %w", err)
	}

	return nil
}
