// helper-gui is the customer-facing front door: a local web UI for
// visually editing config.json (with connection tests), launching and
// stopping helper.exe, and watching its logs. Everything binds to
// 127.0.0.1 only; the browser opens automatically on start.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"tomcat-ops-helper/internal/helpergui"
)

func main() {
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	var (
		configPath = flag.String("config", filepath.Join(exeDir, "config.json"), "path to config.json")
		helperPath = flag.String("helper", filepath.Join(exeDir, "helper.exe"), "path to helper.exe")
		port       = flag.Int("port", helpergui.DefaultPort, "preferred local UI port (falls back to a random free one)")
		noBrowser  = flag.Bool("no-browser", false, "do not auto-open the browser")
	)
	flag.Parse()

	abs, err := filepath.Abs(*configPath)
	if err != nil {
		fatal(err)
	}
	helperAbs, err := filepath.Abs(*helperPath)
	if err != nil {
		fatal(err)
	}

	srv := helpergui.New(abs, helperAbs)

	l, err := helpergui.Listen(*port)
	if err != nil {
		fatal(err)
	}
	url := "http://" + l.Addr().String()

	log.Printf("helper-gui 启动: %s", url)
	log.Printf("配置文件: %s   helper: %s", abs, helperAbs)
	log.Printf("关闭本窗口将同时停止由它启动的 helper。")

	if !*noBrowser {
		if err := helpergui.OpenBrowser(url); err != nil {
			log.Printf("warn: 无法自动打开浏览器(%v),请手动访问上面的地址", err)
		}
	}

	// Ctrl+C and console-window close both arrive here; stop our helper
	// child cleanly before exiting.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigs
		log.Printf("helper-gui: 收到信号 %s,正在停止 helper…", s)
		srv.Shutdown()
		cancel()
	}()
	go func() {
		<-ctx.Done()
		srv.Shutdown()
	}()

	if err := http.Serve(l, srv.Handler()); err != nil {
		if ctx.Err() != nil {
			log.Printf("helper-gui: 已退出")
			return
		}
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "helper-gui fatal:", err)
	os.Exit(1)
}
