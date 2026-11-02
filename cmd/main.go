package main

import (
	"fmt"
	"nexus/raft"
	"os"

	"github.com/spf13/cobra"
)

var (
	host   string
	port   string
	driver string
	dsn    string
)

func RegisterCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "nexus",
	}
	flags := cmd.PersistentFlags()
	flags.StringVar(&host, "host", defaultHost,
		"本节点 IP（对外通告给其他节点，也是监听地址）")
	flags.StringVar(&port, "port", raft.DEFAULTPORT,
		"本节点端口（HTTP 服务与 Raft RPC 共用）")
	cmd.AddCommand(GetRunningCmd())
	cmd.SetHelpCommand(GetHelpCmd())
	return cmd
}

func main() {
	rootCmd := RegisterCommand()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}
