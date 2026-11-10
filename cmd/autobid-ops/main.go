package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/autobid/autobid-engine/internal/ops"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "用法：autobid-ops check [--json] | backup --out DIR | verify --dir DIR | restore --dir DIR --target-database-url-env NAME --data-root DIR")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonMode := fs.Bool("json", false, "")
	dst := fs.String("out", "", "")
	src := fs.String("dir", "", "")
	env := fs.String("target-database-url-env", "", "")
	root := fs.String("data-root", "", "")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		fmt.Fprintln(errOut, "命令参数无效")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	c := ops.FromEnv()
	var e error
	var result any
	switch args[0] {
	case "check":
		r, err := ops.Check(ctx, c)
		e = err
		result = r
		if e == nil && !*jsonMode {
			for _, i := range r.Items {
				label := "就绪"
				if !i.OK {
					label = "待处理"
				}
				fmt.Fprintf(out, "%s [%s] %s", i.Name, label, i.Message)
				if i.FreeBytes != nil {
					fmt.Fprintf(out, "（可用 %d 字节）", *i.FreeBytes)
				}
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "无在途作业：%t。停机后才能取得一致备份。\n", r.CanStop)
		}
		if e == nil {
			bad := false
			for _, i := range r.Items {
				bad = bad || (i.Required && !i.OK)
			}
			if bad {
				if *jsonMode {
					_ = json.NewEncoder(out).Encode(r)
				}
				return 3
			}
		}
	case "backup":
		if *dst == "" {
			fmt.Fprintln(errOut, "请指定 --out")
			return 2
		}
		result, e = ops.Backup(ctx, c, *dst)
	case "verify":
		if *src == "" {
			fmt.Fprintln(errOut, "请指定 --dir")
			return 2
		}
		result, e = ops.Verify(*src)
	case "restore":
		if *src == "" || *env == "" || *root == "" {
			fmt.Fprintln(errOut, "请指定备份、目标连接环境变量名和空数据目录")
			return 2
		}
		e = ops.Restore(ctx, c, *src, os.Getenv(*env), *root)
		result = map[string]any{"restored": e == nil, "key_notice": ops.KeyReminder}
	default:
		fmt.Fprintln(errOut, "未知运维命令")
		return 2
	}
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 1
	}
	if *jsonMode {
		_ = json.NewEncoder(out).Encode(result)
	} else if args[0] != "check" {
		fmt.Fprintln(out, "完成："+args[0])
		fmt.Fprintln(out, ops.KeyReminder)
	}
	return 0
}
