package llm

import "errors"

// stdErrorsAs 直接委托 errors.As；独立函数便于在 transport 内不重复 import。
func stdErrorsAs(err error, target any) bool {
	return errors.As(err, target)
}

// parseRetrySeconds 解析纯数字 Retry-After。
func parseRetrySeconds(v string) (int64, error) {
	var n int64
	var dot bool
	for i, r := range v {
		if r == '.' && !dot && i > 0 {
			dot = true
			continue
		}
		if r < '0' || r > '9' {
			return 0, errors.New("not numeric")
		}
	}
	for _, r := range v {
		if r >= '0' && r <= '9' {
			n = n*10 + int64(r-'0')
		}
	}
	if n == 0 && v != "0" {
		return 0, errors.New("zero")
	}
	return n, nil
}
