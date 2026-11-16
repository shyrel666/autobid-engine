package ops

// R2 本机账号运维：创建第一个管理员、重置口令、启用账号、列出账号。
// 这些命令直接连接数据库，因此需要本机访问权与数据库连接配置；服务端没有任何默认账号或默认口令。
// 命令不执行迁移：数据库必须已由当前版本的服务启动过一次（含 users 表）。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/identity"
)

// UserCommand 是 `autobid-ops user <action>` 的参数。
type UserCommand struct {
	Action        string // create-admin | reset-password | enable | list
	Login         string
	DisplayName   string
	PasswordStdin bool
}

// SecretReader 读取一行口令；交互式终端下不回显。
type SecretReader func(prompt string) (string, error)

func requireUsersTable(ctx context.Context, d *db.DB) error {
	var ok bool
	if err := d.QueryRowContext(ctx, `SELECT to_regclass('users') IS NOT NULL`).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("数据库还没有账号表：请先用当前版本的 start.bat 启动一次服务完成升级，再运行本命令")
	}
	return nil
}

func readPassword(in io.Reader, stdin bool, secret SecretReader, login string) (string, error) {
	var pw string
	if stdin {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		pw = strings.TrimRight(line, "\r\n")
	} else {
		a, err := secret("新口令：")
		if err != nil {
			return "", err
		}
		b, err := secret("再输入一次：")
		if err != nil {
			return "", err
		}
		if a != b {
			return "", errors.New("两次输入的口令不一致")
		}
		pw = a
	}
	if err := identity.CheckPassword(login, pw); err != nil {
		return "", errors.New("口令须为 10~256 个字符，且不能与账号相同")
	}
	return pw, nil
}

func readLine(r io.Reader) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			b = append(b, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(b) > 0 {
				break
			}
			return "", err
		}
	}
	return strings.TrimRight(string(b), "\r"), nil
}

// RunUser 执行账号运维命令，结果写到 out。口令不写入任何输出或日志。
func RunUser(ctx context.Context, c Config, cmd UserCommand, in io.Reader, out io.Writer, secret SecretReader) error {
	d, err := Open(ctx, c.DatabaseURL)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := requireUsersTable(ctx, d); err != nil {
		return err
	}
	switch cmd.Action {
	case "list":
		users, err := d.ListUsers(ctx, db.DefaultTenant)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Fprintln(out, "还没有任何账号；用 ops.bat user create-admin --login <账号> 创建第一个管理员。")
		}
		for _, u := range users {
			flag := ""
			if u.Admin {
				flag = " 管理员"
			}
			fmt.Fprintf(out, "%s（%s）%s 状态=%s\n", u.Login, u.DisplayName, flag, u.Status)
		}
		return nil
	case "create-admin":
		login, err := db.NormalizeLogin(cmd.Login)
		if err != nil {
			return errors.New("请用 --login 指定账号：2~32 位小写字母、数字或 . _ -")
		}
		pw, err := readPassword(in, cmd.PasswordStdin, secret, login)
		if err != nil {
			return err
		}
		hash, err := identity.HashPassword(pw)
		if err != nil {
			return err
		}
		if _, err := d.CreateUser(ctx, db.DefaultTenant, db.NewUser{Login: login, DisplayName: cmd.DisplayName, PasswordHash: hash, Admin: true}, "ops"); err != nil {
			if errors.Is(err, db.ErrAccountConflict) {
				return fmt.Errorf("账号 %s 已存在；如需恢复访问请用 reset-password / enable", login)
			}
			return err
		}
		fmt.Fprintf(out, "已创建管理员 %s\n", login)
		return nil
	case "reset-password":
		u, err := d.GetUserByLogin(ctx, db.DefaultTenant, cmd.Login)
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("账号 %s 不存在", cmd.Login)
		}
		if err != nil {
			return err
		}
		pw, err := readPassword(in, cmd.PasswordStdin, secret, u.Login)
		if err != nil {
			return err
		}
		hash, err := identity.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := d.SetUserPassword(ctx, db.DefaultTenant, u.ID, hash, "password_reset", "ops"); err != nil {
			return err
		}
		fmt.Fprintf(out, "已重置 %s 的口令并解除锁定；该账号原有会话全部失效%s\n", u.Login, map[bool]string{true: "（账号仍为停用状态，需另行 enable）", false: ""}[u.Status != "active"])
		return nil
	case "enable":
		u, err := d.GetUserByLogin(ctx, db.DefaultTenant, cmd.Login)
		if errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("账号 %s 不存在", cmd.Login)
		}
		if err != nil {
			return err
		}
		active := "active"
		if _, err := d.UpdateUser(ctx, db.DefaultTenant, u.ID, db.UserUpdate{Status: &active}, "ops"); err != nil {
			return err
		}
		fmt.Fprintf(out, "已启用 %s\n", u.Login)
		return nil
	default:
		return errors.New("未知的 user 子命令；可用：create-admin、reset-password、enable、list")
	}
}

// adminCheck 是 check 命令里的"管理员账号"项（只读）。
func adminCheck(ctx context.Context, d *db.DB) CheckItem {
	if requireUsersTable(ctx, d) != nil {
		return CheckItem{Name: "管理员账号", OK: false, Message: "数据库尚无账号表；启动当前版本的服务完成升级后，用 ops.bat user create-admin 创建"}
	}
	n, err := d.CountActiveAdmins(ctx, db.DefaultTenant)
	if err != nil {
		return CheckItem{Name: "管理员账号", OK: false, Message: "无法读取账号"}
	}
	if n == 0 {
		return CheckItem{Name: "管理员账号", OK: false, Message: "没有可用的管理员；用 ops.bat user create-admin --login <账号> 创建"}
	}
	c := int64(n)
	return CheckItem{Name: "管理员账号", OK: true, Count: &c, Message: fmt.Sprintf("可用管理员 %d 个", n)}
}
