package deploymcp

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func usageText() string {
	return fmt.Sprintf(`deployment-mcp %s

部署控制面的 MCP 服务。默认用 stdio：Cursor 用管道把标准输入接上协议消息。
加 --http 则在本机另开一个 HTTP MCP，供别的机器连接。
这个命令没有发版子命令，不能在这里直接部署。

用法：
  deployment-mcp                       在标准输入上提供 MCP（Cursor 这样启动）
  deployment-mcp --http 127.0.0.1:4221 提供 HTTP MCP（Streamable HTTP）
  deployment-mcp --help                显示这段说明
  deployment-mcp --version             显示版本

环境变量：
  DEPLOYMENT_URL           默认 http://127.0.0.1:4220
  IDENTITY_ROLE            默认 agent
  IDENTITY_ID              默认 deployment-mcp
  DEPLOYMENT_HTTP_TIMEOUT  默认 60s

在终端里不带参数运行时，会打印这段说明并退出。
只有标准输入是管道时才会进入 MCP 服务。
`, version)
}

// PrepareCLI decides whether to start the stdio server.
// args is os.Args[1:] after flags such as --http are removed. A terminal on
// stdin is not an MCP client, so the process prints usage and does not block.
func PrepareCLI(args []string, stdin io.Reader) (start bool, code int, text string) {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			return false, 0, usageText()
		case "-v", "--version", "version":
			return false, 0, fmt.Sprintf("deployment-mcp %s\n", version)
		default:
			return false, 2, fmt.Sprintf("未知参数 %q\n\n%s", args[0], usageText())
		}
	}
	if f, ok := stdin.(*os.File); ok && isCharDevice(f) {
		return false, 0, usageText()
	}
	return true, 0, ""
}

// SplitHTTPFlag pulls out `--http addr`. The address is the listen address
// of a standalone Streamable HTTP MCP server.
func SplitHTTPFlag(args []string) (addr string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] != "--http" {
			rest = append(rest, args[i])
			continue
		}
		if addr != "" {
			return "", nil, fmt.Errorf("--http 重复")
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
			return "", nil, fmt.Errorf("--http 需要监听地址，例如 127.0.0.1:4221")
		}
		i++
		addr = args[i]
	}
	return addr, rest, nil
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
