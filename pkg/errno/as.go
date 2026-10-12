package errno

import "errors"

// as 是对 errors.As 的薄封装，单独抽出便于测试替换。
func as(err error, target any) bool {
	return errors.As(err, target)
}
