package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// GetHelpCmd returns the help command. With no arguments it prints a
// step-by-step configuration guide; with arguments it falls back to the
// standard help of the referenced command.
func GetHelpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "查看参数说明与配置指引",
		Long:  "输出启动节点需要提供的参数、可选的环境变量以及单机/多节点组网示例。",
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) > 0 {
				target, _, err := cmd.Root().Find(args)
				if err != nil || target == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "未知命令: %s\n\n", strings.Join(args, " "))
					_ = cmd.Root().Help()
					return
				}
				_ = target.Help()
				return
			}
			printConfigGuide(cmd.OutOrStdout())
		},
	}
}

func printConfigGuide(w io.Writer) {
	fmt.Fprint(w, `Nexus Raft 配置中心 —— 启动配置指引

一、必须提供的参数（启动节点时至少确认这两项）
  --host    本节点的 IP，既是监听地址，也是通告给其他节点的地址
            例如：--host 192.168.1.10（默认 127.0.0.1）
  --port    本节点端口，HTTP 服务与 Raft RPC 共用
            例如：--port 11451（默认 11451）

二、可选参数
  --db-driver   数据库驱动，目前支持 sqlite3（默认 sqlite3，可被 RAFT_DB_DRIVER 覆盖）
  --db-dsn      数据库文件路径或 DSN（默认 config.db，可被 RAFT_DB_DSN 覆盖）
                多节点测试时建议每个节点使用独立的 db 文件

三、推荐提供的环境变量
  RAFT_AUTH_TOKEN   管理员 token。不设置时首次启动会自动生成并持久化到数据库
  RAFT_NODE_IPS     首次启动时的种子节点，逗号分隔，可省略端口（默认 11451）
                    例如：RAFT_NODE_IPS=10.0.0.1:11451,10.0.0.2:11451

四、单节点启动示例
  ./nexus run --host 127.0.0.1 --port 11451 --db-dsn node1.db

五、三节点集群启动示例
  每个节点执行一次，使用独立的 db 文件；RAFT_AUTH_TOKEN 需保持一致。

  RAFT_NODE_IPS=10.0.0.1:11451,10.0.0.2:11451,10.0.0.3:11451 \
  RAFT_AUTH_TOKEN=your-token \
  ./nexus run --host 10.0.0.1 --port 11451 --db-dsn node1.db

  RAFT_NODE_IPS=10.0.0.1:11451,10.0.0.2:11451,10.0.0.3:11451 \
  RAFT_AUTH_TOKEN=your-token \
  ./nexus run --host 10.0.0.2 --port 11451 --db-dsn node2.db

  RAFT_NODE_IPS=10.0.0.1:11451,10.0.0.2:11451,10.0.0.3:11451 \
  RAFT_AUTH_TOKEN=your-token \
  ./nexus run --host 10.0.0.3 --port 11451 --db-dsn node3.db

  说明：节点启动时会把自身的 host:port 写入 node_configs 表，并以该表为准
  组装 Raft 网络；运行期间也可以调用 POST /api/v1/nodes 动态增删节点。

六、验证与鉴权
  访问 http://<host>:<port>/ 打开配置中心页面；
  接口请求需携带管理员 token：
    curl -H "Auth: <token>" http://<host>:<port>/api/v1/nodes

查看某个子命令的详细参数：nexus help run
`)
}
