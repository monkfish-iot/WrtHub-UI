// Command hashpw 生成 bcrypt 密码 hash，用于命令行重置用户密码。
//
// 用法（在项目根）：
//
//	go run ./tools/hashpw 'NewStrongPass!2026'
//
// 输出 hash 后用 sqlite3 更新：
//
//	sqlite3 data/wrthub.db "UPDATE users SET password_hash='<hash>' WHERE username='admin';"
//
// 复用 internal/store.HashPassword（bcrypt.DefaultCost），与登录验证算法一致。
package main

import (
	"fmt"
	"os"

	"wrthub-ui/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/hashpw <password>")
		os.Exit(2)
	}
	hash, err := store.HashPassword(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "hash:", err)
		os.Exit(1)
	}
	fmt.Println(hash)
}
