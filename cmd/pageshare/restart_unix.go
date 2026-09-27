//go:build unix

package main

import (
	"os"
	"syscall"
)

// restartSelf 以相同参数与环境变量重启自身（unix）：execve 原地替换进程映像，
// 监听端口随 CLOEXEC 释放，新进程重新加载已落盘的配置进入正常模式。
func restartSelf() error {
	return syscall.Exec(os.Args[0], os.Args, os.Environ())
}
