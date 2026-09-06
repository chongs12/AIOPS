// Package auth 提供登录鉴权：JWT(HS256) + admin_users 表 + gin 中间件。
// 默认关闭（auth.enabled=false），启用后保护管理面与变更类操作。
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// ErrUnauthorized 鉴权失败。
var ErrUnauthorized = errors.New("auth: 未认证或令牌失效")

// Service 鉴权服务。
type Service struct {
	db     *sql.DB
	secret []byte
	ttl    time.Duration
}

// Claims 令牌载荷。
type Claims struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// New 构造并迁移 + seed 默认账号（admin/admin123，仅当表为空）。
func New(ctx context.Context, db *sql.DB, secret string, ttl time.Duration) (*Service, error) {
	const schema = `
CREATE TABLE IF NOT EXISTS admin_users (
    username   TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'admin',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("admin_users 迁移: %w", err)
	}
	s := &Service{db: db, secret: []byte(secret), ttl: ttl}
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n)
	if n == 0 {
		hash, err := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		_, _ = db.ExecContext(ctx,
			`INSERT INTO admin_users (username, password_hash, role) VALUES ('admin', $1, 'admin')`, hash)
		slog.Info("已 seed 默认管理员 admin/admin123（请尽快修改）")
	}
	return s, nil
}

// Login 校验账号密码并签发 JWT。
func (s *Service) Login(ctx context.Context, username, password string) (token string, err error) {
	var hash, role string
	err = s.db.QueryRowContext(ctx,
		`SELECT password_hash, role FROM admin_users WHERE username=$1`, username).
		Scan(&hash, &role)
	if err != nil {
		return "", ErrUnauthorized
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", ErrUnauthorized
	}
	claims := Claims{
		Username: username, Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// Parse 校验令牌。
func (s *Service) Parse(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrUnauthorized
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, ErrUnauthorized
	}
	return claims, nil
}

// Middleware gin 鉴权中间件：Authorization: Bearer <token>。
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			c.AbortWithStatusJSON(401, gin.H{"error": "未认证"})
			return
		}
		claims, err := s.Parse(strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			c.AbortWithStatusJSON(401, gin.H{"error": "令牌失效"})
			return
		}
		c.Set("username", claims.Username)
		c.Set("role", claims.Role)
		c.Next()
	}
}
