//go:build !unix

package main

import "errors"

// restartSelf 非 unix 平台（Windows）不支持 execve 自重启，由调用方提示手动重启。
func restartSelf() error {
	return errors.New("当前平台不支持自动重启")
}
