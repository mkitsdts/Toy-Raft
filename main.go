package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"nexus/configcenter"
	"nexus/raft"
	"os"
	"os/signal"
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
	// 把环境变量里配置的节点写入注册表（已存在的不会重复插入），
	// 之后节点列表以数据库为准，可在配置中心动态增删。
	if err := store.SeedNodes(cfg.NodeIPs, raft.DEFAULTPORT); err != nil {
		log.Fatalf("初始化 Raft 节点列表失败: %v", err)
	}
	nodes, err := store.ListNodes()
	if err != nil {
		log.Fatalf("加载 Raft 节点列表失败: %v", err)
	}
	addresses := make([]string, 0, len(nodes))
	for _, n := range nodes {
		addresses = append(addresses, net.JoinHostPort(n.IP, n.Port))
	}
	node := raft.InitRaftNode(cfg.LocalIP, addresses)
	if err := node.SetStorage(store); err != nil {
		log.Fatalf("加载 Raft 日志失败: %v", err)
	}
	// 状态机：日志提交后在每个节点上执行，把命令写入配置表。节点成员变化
	// 还需要刷新本节点的 peer 列表；apply 运行时不持有 Raft 锁，调用安全。
	node.SetApplyFunc(func(cmd raft.Command) error {
		if err := store.ApplyCommand(cmd); err != nil {
			return err
		}
		if cmd.OpType == configcenter.OpNodeRegister || cmd.OpType == configcenter.OpNodeDelete {
			refreshNodePeers(store, node)
		}
		return nil
	})
	fmt.Println("Raft 节点初始化完成，节点 IP:", node.LocalIP)

	mux := http.NewServeMux()
	node.RegisterRoutes(mux)
	configServer := configcenter.NewServer(store, cfg.AuthToken)
	// 动态注册/删除节点后，立即刷新 Raft 的 peer 列表。
	configServer.SetNodeRegistrar(node)
	// 配置写请求走 Raft 日志复制。
	configServer.SetRaft(node)
	configServer.RegisterRoutes(mux)
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

// refreshNodePeers reloads the registered node list and pushes it to the local
// Raft node, so membership commands take effect on every node that applies
// them (not just the one that received the HTTP request).
func refreshNodePeers(store *configcenter.Store, node *raft.RaftNode) {
	nodes, err := store.ListNodes()
	if err != nil {
		return
	}
	addrs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		addrs = append(addrs, net.JoinHostPort(n.IP, n.Port))
	}
	node.SetNodes(addrs)
}
