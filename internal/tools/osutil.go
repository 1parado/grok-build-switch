package tools

import "os"

// osReadDir 是 os.ReadDir 的包级别名（分离到独立文件便于测试替身）。
func osReadDir(dir string) ([]os.DirEntry, error) {
	return os.ReadDir(dir)
}
