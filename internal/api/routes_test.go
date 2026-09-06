// 路由完整性测试：每个导出的 handler 方法都必须挂载到路由上（防"写了 handler 忘挂路由"复发）。
package api

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAllHandlersAreRouted(t *testing.T) {
	// 反射列出 *Server 上全部 handler 方法（签名 gin.HandlerFunc）。
	handlerNames := map[string]bool{}
	typ := ginHandlerType()
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		// 只统计 gin.HandlerFunc 形态的方法（1 入参 *gin.Context，无出参）。
		mt := m.Type
		if mt.NumIn() != 2 || mt.NumOut() != 0 {
			continue
		}
		if mt.In(1) != reflect.TypeOf(&gin.Context{}) {
			continue
		}
		handlerNames[m.Name] = true
	}

	// 构造最小可用 Server 的路由（nil 依赖即可，路由注册不触发依赖）。
	gin.SetMode(gin.TestMode)
	s2 := &Server{}
	r := gin.New()
	_ = r // 占位：实际路由在 s2.Handler() 中注册；此处用真实 Handler 收集已注册路径。
	engine := s2.Handler().(*gin.Engine)
	registered := map[string]bool{}
	for _, ri := range engine.Routes() {
		registered[handlerShortName(ri.Handler)] = true
	}

	// 条件注册的路由（仅 auth.enabled 时挂载）——白名单豁免。
	conditional := map[string]bool{"Login": true, "Me": true}

	// 逐一断言：每个非条件 handler 方法名都必须出现在已注册路由中。
	for name := range handlerNames {
		if conditional[name] {
			continue
		}
		if !registered[name] {
			t.Errorf("handler Server.%s 未挂载到任何路由（写了 handler 忘挂路由）", name)
		}
	}

	// 反向断言条件路由：注入 auth 后必须出现（防条件注册块整体丢失）。
	s3 := &Server{}
	s3.WithAuth(stubAuth{})
	engine3 := s3.Handler().(*gin.Engine)
	authed := map[string]bool{}
	for _, ri := range engine3.Routes() {
		authed[handlerShortName(ri.Handler)] = true
	}
	t.Logf("启用鉴权后路由数: %d", len(authed))
	if !authed["Login"] && !authed["Me"] {
		t.Errorf("条件 handler Login/Me 在启用鉴权时均未挂载（auth 条件块未生效）")
	}
}

// stubAuth 测试用鉴权后端。
type stubAuth struct{}

func (stubAuth) Login(_ context.Context, _, _ string) (string, error) { return "t", nil }
func (stubAuth) Middleware() gin.HandlerFunc                          { return func(c *gin.Context) { c.Next() } }

// handlerShortName 从 gin 路由的 Handler 字段（含包路径/-fm 后缀）提取短方法名。
func handlerShortName(h string) string {
	if i := strings.LastIndexByte(h, '.'); i >= 0 {
		h = h[i+1:]
	}
	if i := strings.IndexByte(h, '-'); i >= 0 {
		h = h[:i]
	}
	return h
}

// ginHandlerType 反射取 *Server 类型。
func ginHandlerType() reflect.Type {
	return reflect.TypeOf(&Server{})
}
