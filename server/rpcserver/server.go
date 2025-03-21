package rpcserver

import (
	"context"
	"fmt"
	"goken/pkg/common/hostgen"
	errors "goken/pkg/errors"
	"goken/pkg/log"
	"goken/registry"
	sinterceptors "goken/server/rpcserver/sinterceptors"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/oklog/run"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

type ServerOption func(o *Server)

type Server struct {
	*grpc.Server
	//如果lis不为空就使用传入的lis作为地址,否则默认使用tcp与host构成lis
	Host       string
	UnaryInts  []grpc.UnaryServerInterceptor
	StreamInts []grpc.StreamServerInterceptor
	GrpcOpts   []grpc.ServerOption
	Lis        net.Listener
	Ctx        context.Context

	Registor registry.Registor
	//timeout  time.Duration
	Health   *health.Server
	Instance *registry.ServiceInstance
	closed   bool
}

var ErrNilRpcRegistor = errors.New("该rpc服务不存在注册器")

func (s *Server) listen() error {

	if s.Lis != nil {
		s.Host = s.Lis.Addr().String()
		return nil
	}
	//检查并获得合适的地址用于服务注册
	addr, err := hostgen.ResolveHost(s.Host)
	if err != nil {
		return err
	}
	//s.endpoint = &url.URL{Scheme: "grpc", Host: addr}
	s.Host = addr
	s.Lis, _ = net.Listen("tcp", s.Host)
	return nil
}

func MustNewServer(ctx context.Context, opts ...ServerOption) *Server {
	s := &Server{
		Host:      "127.0.0.1:0",
		Health:    health.NewServer(),
		Ctx:       ctx,
		Instance:  new(registry.ServiceInstance),
		UnaryInts: []grpc.UnaryServerInterceptor{sinterceptors.HealthCheckInterceptor()},
		closed:    false,
	}
	for _, v := range opts {
		v(s)
	}

	if err := s.listen(); err != nil {
		panic(err)
	}

	if s.Instance.ID == "" {
		s.Instance.ID = s.Host
	}
	if s.Instance.Name == "" {
		s.Instance.Name = s.Host
	}

	u, err := url.Parse(fmt.Sprintf("%s://%s", "grpc", s.Host))
	if err != nil {
		panic(err)
	}
	s.Instance.Endpoints = append(s.Instance.Endpoints, u)

	//s.unaryInts = append(s.unaryInts, interceptors.UnaryTimeoutInterceptor(s.timeout))

	s.GrpcOpts = append(s.GrpcOpts, grpc.ChainUnaryInterceptor(s.UnaryInts...))
	s.GrpcOpts = append(s.GrpcOpts, grpc.ChainStreamInterceptor(s.StreamInts...))
	s.Server = grpc.NewServer(s.GrpcOpts...)

	grpc_health_v1.RegisterHealthServer(s.Server, s.Health)
	//用于在运行时暴露gRPC服务的元数据信息,提供对gRPC服务的反射能力,使得客户端能够在没有事先了解服务定义的情况下,动态地查询服务和方法
	reflection.Register(s.Server)
	return s
}

func (s *Server) Register(ctx context.Context, ins *registry.ServiceInstance) error {
	if s.Registor == nil {
		return ErrNilRpcRegistor
	}
	return s.Registor.Register(ctx, ins)
}

// Deregister会注销Server内Instance存储的服务Id
func (s *Server) Deregister(ctx context.Context) error {
	if s.Registor == nil {
		return ErrNilRpcRegistor
	}
	if !s.closed {
		s.closed = true
		return s.Registor.Deregister(ctx, s.Instance.ID)
	}
	return nil
}

func (s *Server) Serve() error {
	g := &run.Group{}
	//运行前前打印配置信息
	log.Infof("[rpcserver] 服务启动中,监听信息为: host = %s,服务信息为: msg = %+v", s.Host, s.Instance)
	//如果注册器为空就不进行注册而不是返回错误,
	if err := s.Register(s.Ctx, s.Instance); err != nil && err != ErrNilRpcRegistor {
		return err
	}

	// 确保Deregister只执行一次
	var deregisterOnce sync.Once
	deregisterFunc := func() {
		deregisterOnce.Do(func() {
			if e := s.Deregister(s.Ctx); e != nil && e != ErrNilRpcRegistor {
				log.Errorf("[httpserver] 服务注销失败, err= %v", e)
			} else {
				log.Info("[httpserver] 服务正常注销")
			}
		})
	}

	//监听终止信号,优雅退出
	sign := make(chan os.Signal, 1)
	signal.Notify(sign, syscall.SIGTERM, syscall.SIGINT)
	g.Add(
		func() error {
			if err := s.Server.Serve(s.Lis); err != nil {
				log.Errorf("[rpcserver] 服务启动失败, err= %v", err)
				return err
			}
			return nil
		},
		func(err error) {
			s.Server.GracefulStop()
			deregisterFunc()
		},
	)

	g.Add(
		func() error {

			<-sign
			s.Server.GracefulStop()
			deregisterFunc()
			return nil

		},
		func(err error) {
			sign <- syscall.SIGINT
		},
	)
	return g.Run()
}

func WithHost(host string) ServerOption {
	return func(o *Server) {
		o.Host = host
	}
}

func WithTimeout(timeout time.Duration) ServerOption {
	return func(o *Server) {
		o.UnaryInts = append(o.UnaryInts, sinterceptors.UnaryTimeoutInterceptor(timeout))
	}
}

func WithListener(lis net.Listener) ServerOption {
	return func(o *Server) {
		o.Lis = lis
	}
}

func WithUnaryInts(ui ...grpc.UnaryServerInterceptor) ServerOption {
	return func(o *Server) {
		o.UnaryInts = append(o.UnaryInts, ui...)
	}
}

func WithSteamInts(sui ...grpc.StreamServerInterceptor) ServerOption {
	return func(o *Server) {
		o.StreamInts = append(o.StreamInts, sui...)
	}
}

func WithGrpcOptions(opts ...grpc.ServerOption) ServerOption {
	return func(o *Server) {
		o.GrpcOpts = opts
	}
}

func WithRegistor(r registry.Registor) ServerOption {
	return func(o *Server) {
		o.Registor = r
	}
}

// func WithServiceInstance(ins *registry.ServiceInstance) ServerOption {
// 	return func(o *Server) {
// 		o.Instance = ins
// 	}
// }

func WithServiceName(name string) ServerOption {
	return func(o *Server) {
		o.Instance.Name = name
	}
}

func WithServiceID(id string) ServerOption {
	return func(o *Server) {
		o.Instance.ID = id
	}
}

func WithVersion(v string) ServerOption {
	return func(o *Server) {
		o.Instance.Version = v
	}
}
