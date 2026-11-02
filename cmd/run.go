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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const defaultHost = "127.0.0.1"

// options carries the flags of the root command.
type options struct {
	host     string
	port     string
	dbDriver string
	dbDSN    string
}

func (o *options) validate() error {
	o.host = strings.TrimSpace(o.host)
	if o.host == "" {
		return errors.New("--host 不能为空")
	}
	if ip := net.ParseIP(o.host); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("--host 必须是其他节点可达的地址，不能是 %s", o.host)
	}

	port, err := strconv.Atoi(strings.TrimSpace(o.port))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("--port 必须是 1-65535 之间的整数，当前值 %q", o.port)
	}
	o.port = strconv.Itoa(port)
	return nil
}

func GetRunningCmd() *cobra.Command {
	dbCfg := configcenter.DatabaseConfigFromEnv()
	opts := &options{}

	cmd := &cobra.Command{
		Use:           "run",
		Short:         "Raft 配置中心",
		Long:          "Nexus 是一个基于 Raft 的配置中心，所有节点组成集群并复制同一份配置日志。",
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, _ []string) {
			if err := opts.validate(); err != nil {
				log.Fatal(err)
			}

			ctx := cmd.Context()
			selfAddr := net.JoinHostPort(opts.host, opts.port)

			store, err := configcenter.OpenStore(configcenter.DatabaseConfig{
				Driver: opts.dbDriver,
				DSN:    opts.dbDSN,
			})
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
			// 环境变量里配置的节点仅用于首次引导（已存在的不会重复插入），
			// 之后节点列表以数据库为准，可在配置中心动态增删。
			if err := store.SeedNodes(cfg.NodeIPs, raft.DEFAULTPORT); err != nil {
				log.Fatalf("初始化 Raft 节点列表失败: %v", err)
			}

			// 把自己登记进节点表，这样每个节点启动后都会出现在集群成员列表里。
			if _, err := store.RegisterNode(opts.host, opts.port); err != nil {
				log.Fatalf("登记本节点失败: %v", err)
			}
			if err := store.SetLocalIP(selfAddr); err != nil {
				log.Fatal(err)
			}

			// 以数据库中的节点表为准组装 Raft 网络。
			nodes, err := store.ListNodes()
			if err != nil {
				log.Fatalf("加载 Raft 节点列表失败: %v", err)
			}
			addresses := make([]string, 0, len(nodes))
			for _, n := range nodes {
				addresses = append(addresses, n.Address())
			}

			node, err := configcenter.NewNode(store, selfAddr, addresses)
			if err != nil {
				log.Fatalf("初始化 Raft 节点失败: %v", err)
			}
			fmt.Printf("Raft 节点初始化完成，本节点 %s，集群成员 %v\n", selfAddr, node.Nodes())

			mux := http.NewServeMux()
			node.RegisterRoutes(mux)
			configServer := configcenter.NewServer(store, cfg.AuthToken)
			// 配置写请求走 Raft 日志复制。
			configServer.SetRaft(node)
			configServer.RegisterRoutes(mux)
			node.Run()

			// 先同步监听，确保端口绑定成功后再进入信号等待，避免启动竞争。
			listener, err := net.Listen("tcp", selfAddr)
			if err != nil {
				node.Stop()
				log.Fatalf("监听 %s 失败: %v", selfAddr, err)
			}
			server := &http.Server{Handler: mux}

			// Ctrl+C（SIGINT）或 SIGTERM 时取消 context，触发优雅关闭。
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			serveErr := make(chan error, 1)
			go func() {
				fmt.Println("Raft 配置中心启动完成，监听", selfAddr)
				serveErr <- server.Serve(listener)
			}()

			select {
			case <-ctx.Done():
				fmt.Println("\n收到退出信号，正在优雅关闭...")
			case err := <-serveErr:
				node.Stop()
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
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.host, "host", defaultHost,
		"本节点 IP（对外通告给其他节点，也是监听地址）")
	flags.StringVar(&opts.port, "port", raft.DEFAULTPORT,
		"本节点端口（HTTP 服务与 Raft RPC 共用）")
	flags.StringVar(&opts.dbDriver, "db-driver", dbCfg.Driver,
		"数据库驱动（默认取 RAFT_DB_DRIVER）")
	flags.StringVar(&opts.dbDSN, "db-dsn", dbCfg.DSN,
		"数据库 DSN（默认取 RAFT_DB_DSN）")

	return cmd
}
