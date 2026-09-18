package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"raft/configcenter"
	"raft/raft"
	"syscall"
	"time"
)

func main() {
	store, err := configcenter.OpenStore(configcenter.DatabaseConfigFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	cfg, err := store.LoadOrInitializeRuntimeConfig()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.GeneratedToken {
		fmt.Println("======================================================")
		fmt.Println("未检测到 RAFT_AUTH_TOKEN，已自动生成管理员 token（拥有全部权限）:")
		fmt.Println("    " + cfg.AuthToken)
		fmt.Println("请在请求头中使用: Auth: " + cfg.AuthToken)
		fmt.Println("======================================================")
	} else {
		fmt.Println("admin token: " + cfg.AuthToken)
	}

	fmt.Println("初始化 Raft 节点...")
	node := raft.InitRaftNode(cfg.LocalIP, cfg.NodeIPs)
	if err := node.SetStorage(store); err != nil {
		log.Fatalf("加载 Raft 日志失败: %v", err)
	}
	fmt.Println("Raft 节点初始化完成，节点 IP:", node.LocalIP)

	mux := http.NewServeMux()
	node.RegisterRoutes(mux)
	configcenter.NewServer(store, cfg.AuthToken).RegisterRoutes(mux)
	node.Run()

	address := ":" + raft.DEFAULTPORT
	// 先同步监听，确保端口绑定成功后再进入信号等待，避免启动竞争。
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", address, err)
	}
	server := &http.Server{Handler: mux}

	// Ctrl+C（SIGINT）或 SIGTERM 时取消 context，触发优雅关闭。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		fmt.Println("Raft 配置中心启动完成，监听", address)
		serveErr <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		fmt.Println("\n收到退出信号，正在优雅关闭...")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
		return
	}

	// 先停止 Raft 后台选举/心跳循环，再关闭 HTTP 服务（等待进行中的请求结束）。
	node.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("优雅关闭超时，强制关闭: %v", err)
		_ = server.Close()
	}
	fmt.Println("已退出。")
}
