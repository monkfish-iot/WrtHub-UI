package store

import "golang.org/x/crypto/bcrypt"

// HashPassword 用 bcrypt 生成密码哈希。cost 取 bcrypt.DefaultCost(10)。
// 明文密码仅在此临时变量中，不落盘、不进日志。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ComparePassword 校验明文密码与哈希是否匹配。
func ComparePassword(hash, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}
