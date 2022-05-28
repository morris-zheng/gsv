package server

import (
	"context"
	"errors"
	"fmt"
	grpcMiddleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/ringbrew/gsv/discovery"
	"github.com/ringbrew/gsv/logger"
	"github.com/ringbrew/gsv/service"
	"github.com/ringbrew/gsv/tracex"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"
	"log"
	"net"
	"net/http"
	"sync"
)

type grpcServer struct {
	host               string
	port               int
	proxyPort          int
	gSrv               *grpc.Server
	streamInterceptors []grpc.StreamServerInterceptor
	unaryInterceptors  []grpc.UnaryServerInterceptor
	statHandler        stats.Handler
	register           discovery.Register
	traceOption        tracex.Option

	enableGateway bool
	gSrvGateway   *http.Server
	gatewayMux    *runtime.ServeMux
	serviceList   []service.Service
}

func newGrpcServer(opt Option) *grpcServer {
	s := &grpcServer{
		port:               opt.Port,
		proxyPort:          opt.ProxyPort,
		streamInterceptors: opt.StreamInterceptors,
		unaryInterceptors:  opt.UnaryInterceptors,
		statHandler:        opt.StatHandler,
		register:           opt.ServerRegister,
		enableGateway:      opt.EnableGrpcGateway,
		traceOption:        opt.TraceOption,
	}

	s.host = s.findListenOn()

	opts := make([]grpc.ServerOption, 0)

	if len(s.unaryInterceptors) > 0 {
		opts = append(opts, grpcMiddleware.WithUnaryServerChain(opt.UnaryInterceptors...))
	}

	if len(s.streamInterceptors) > 0 {
		opts = append(opts, grpcMiddleware.WithStreamServerChain(opt.StreamInterceptors...))
	}

	if s.statHandler != nil {
		opts = append(opts, grpc.StatsHandler(opt.StatHandler))
	}

	s.gSrv = grpc.NewServer(opts...)

	if s.enableGateway {
		m := runtime.NewServeMux()
		httpMux := http.NewServeMux()
		httpMux.Handle("/", m)

		hs := &http.Server{
			Addr:    fmt.Sprintf(":%d", s.proxyPort),
			Handler: s.recoverMiddleware(s.traceMiddleware(httpMux)),
		}

		s.gatewayMux = m
		s.gSrvGateway = hs
	}

	return s
}

func (gs *grpcServer) recoverMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if r := recover(); r != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(fmt.Sprintf("%v", r)))
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func (gs *grpcServer) traceMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//todo pass the trace info.
		h.ServeHTTP(w, r)
	})
}

func (gs *grpcServer) Register(srv service.Service) error {
	desc := srv.Description()
	if !desc.Valid {
		return errors.New("invalid service description")
	}

	if len(desc.GrpcServiceDesc) == 0 {
		return errors.New("not invalid grpc service desc")
	}

	gs.serviceList = append(gs.serviceList, srv)

	return nil
}

func (gs *grpcServer) Run(ctx context.Context) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	conn, err := grpc.Dial(fmt.Sprintf("127.0.0.1:%d", gs.port), opts...)
	if err != nil {
		log.Fatal(err.Error())
	}

	for i := range gs.serviceList {
		desc := gs.serviceList[i].Description()

		gs.gSrv.RegisterService(&desc.GrpcServiceDesc[i], gs.serviceList[i])

		for _, f := range desc.GrpcGateway {
			if err := f(context.Background(), gs.gatewayMux, conn); err != nil {
				log.Fatal(err.Error())
			}
		}
	}

	wg := sync.WaitGroup{}

	if gs.enableGateway {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gs.runGateway(ctx); err != nil {
				log.Fatal(fmt.Errorf("server gateway run error:%s", err.Error()))
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := gs.run(ctx); err != nil {
			log.Fatal(fmt.Errorf("server run error:%s", err.Error()))
		}
	}()

	wg.Wait()
}

func (gs *grpcServer) run(ctx context.Context) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", gs.port))
	if err != nil {
		return err
	}

	go func() {
		select {
		case <-ctx.Done():
			logger.Info(logger.NewEntry().WithMessage(fmt.Sprintf("rpc server stop listen on: [%d]", gs.port)))
			gs.gSrv.GracefulStop()
		}
	}()

	if gs.register != nil {
		for i := range gs.serviceList {
			node := discovery.NewNode(gs.host, gs.port, discovery.GRPC, gs.serviceList[i])
			if err := gs.register.Register(node); err != nil {
				return err
			}
		}
	}

	logger.Info(logger.NewEntry().WithMessage(fmt.Sprintf("rpc server start listen on: [%d]", gs.port)))

	if err := gs.gSrv.Serve(lis); err != nil {
		return err
	}

	return nil
}

func (gs *grpcServer) runGateway(ctx context.Context) error {
	if gs.gSrvGateway == nil {
		return nil
	}

	go func() {
		<-ctx.Done()
		logger.Info(logger.NewEntry().WithMessage(fmt.Sprintf("rpc server gateway stop listen on: [%d]", gs.proxyPort)))

		if err := gs.gSrvGateway.Shutdown(context.Background()); err != nil {
			logger.Error(logger.NewEntry().WithMessage(fmt.Sprintf("failed to shutdown http server: %s", err.Error())))
		}
	}()

	if gs.register != nil {
		for i := range gs.serviceList {
			node := discovery.NewNode(gs.host, gs.proxyPort, discovery.HTTP, gs.serviceList[i])
			if err := gs.register.Register(node); err != nil {
				return err
			}
		}
	}
	logger.Info(logger.NewEntry().WithMessage(fmt.Sprintf("rpc server gateway start listen on: [%d]", gs.proxyPort)))

	if err := gs.gSrvGateway.ListenAndServe(); err != http.ErrServerClosed {
		//s.l.Panic(logger.NewEntry().WithMessage(fmt.Sprintf("failed to listen and serve: %s", err.Error())).End())
		return err
	}

	return nil
}

func (gs *grpcServer) findListenOn() string {
	return ""
}