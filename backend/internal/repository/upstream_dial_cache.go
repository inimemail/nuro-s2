package repository

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"
)

// 本文件实现上游连接的两项冷连接加速能力，二者都对功能零影响、仅作用于"新建连接"：
//
//  1. 进程内 DNS 缓存（cachedDialer）：避免连接池 miss 时每次新建连接都做一次
//     DNS 解析。跨机房 / 容器内 DNS 往往 20-80ms，缓存命中后省掉这段。
//     任何解析或拨号异常都会回退到标准 net.Dialer 行为，不改变语义。
//
//  2. 共享 TLS ClientSessionCache：开启 TLS 会话复用（session resumption），
//     普通 TLS transport 的热账号第二条连接起省掉一次完整 TLS 握手 RTT。

const (
	// dnsCacheTTL DNS 缓存正常条目存活时间。
	dnsCacheTTL = 60 * time.Second
	// dnsCacheNegativeTTL 解析失败时的短缓存，避免对挂掉的域名反复打 DNS。
	dnsCacheNegativeTTL = 5 * time.Second
	// dnsDialTimeout 单次拨号超时（与 net/http 默认 DialContext 保持一致）。
	dnsDialTimeout = 30 * time.Second
	// dnsKeepAlive TCP keep-alive 间隔（与 net/http 默认保持一致）。
	dnsKeepAlive = 30 * time.Second
	// keepWarmInterval 后台连接保活周期。略小于 IdleConnTimeout(90s)，
	// 保证空闲连接被回收前 TLS 会话已被预热。
	keepWarmInterval = 60 * time.Second
	// keepWarmDialTimeout 单次保活拨号 + 握手超时。
	keepWarmDialTimeout = 10 * time.Second
	// dnsHappyEyeballsDelay 缓存 IP 拨号时启动下一个候选地址的间隔。
	dnsHappyEyeballsDelay = 300 * time.Millisecond
	// dnsMaxDialCandidates 限制单次缓存拨号最多尝试的 IP 数，避免异常 DNS 结果制造连接风暴。
	dnsMaxDialCandidates = 6
)

// sharedUpstreamTLSSessionCache 是所有上游 Transport 共享的 TLS 会话缓存。
// 传 0 使用默认容量。跨账号共享是安全的：TLS session ticket 与具体连接/账号无关，
// 只与目标 host 关联，复用只影响握手，不会串号、不影响鉴权或计费。
var sharedUpstreamTLSSessionCache = tls.NewLRUClientSessionCache(0)

type dnsCacheEntry struct {
	ips    []net.IPAddr
	expiry time.Time
	err    error
}

// cachedDialer 包一层带 TTL 的 DNS 缓存。命中时直接用缓存 IP 拨号，
// 未命中 / 过期时回源解析并写缓存。任何环节出错都退化为标准拨号。
type cachedDialer struct {
	dialer   *net.Dialer
	resolver *net.Resolver

	mu    sync.RWMutex
	cache map[string]dnsCacheEntry

	// seenHosts 记录实际拨号过的直连上游 host，供后台保活预热使用。
	seenHosts sync.Map // map[string]struct{}
	warmOnce  sync.Once
}

var sharedCachedDialer = newCachedDialer()

func newCachedDialer() *cachedDialer {
	return &cachedDialer{
		dialer: &net.Dialer{
			Timeout:   dnsDialTimeout,
			KeepAlive: dnsKeepAlive,
		},
		resolver: net.DefaultResolver,
		cache:    make(map[string]dnsCacheEntry),
	}
}

// DialContext 是给 http.Transport.DialContext 使用的入口。
func (d *cachedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// 首次拨号时惰性启动后台保活（DNS + TLS 会话预热），全程不发 HTTP 请求。
	d.warmOnce.Do(func() { go d.runKeepWarm() })

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// 地址格式异常，交给标准拨号处理（保持原始错误语义）。
		return d.dialer.DialContext(ctx, network, addr)
	}

	// 已是 IP 字面量，无需解析，直接拨号。
	if ip := net.ParseIP(host); ip != nil {
		return d.dialer.DialContext(ctx, network, addr)
	}

	// 记录 host:port，供后台保活预热（仅记录直连拨号到的目标）。
	d.seenHosts.Store(addr, struct{}{})

	ips, lookupErr := d.lookup(ctx, host)
	if lookupErr != nil || len(ips) == 0 {
		// 缓存里是负结果或解析为空：回退到标准拨号（让其走系统解析 + 产生准确错误）。
		return d.dialer.DialContext(ctx, network, addr)
	}

	conn, cachedDialErr := d.dialCachedIPs(ctx, network, port, ips)
	if cachedDialErr == nil {
		return conn, nil
	}

	// 缓存 IP 全部拨号失败：可能 IP 已变，丢弃缓存并回退标准拨号重试一次。
	d.invalidate(host)
	conn, dialErr := d.dialer.DialContext(ctx, network, addr)
	if dialErr != nil && cachedDialErr != nil {
		return nil, dialErr
	}
	return conn, dialErr
}

func (d *cachedDialer) dialCachedIPs(ctx context.Context, network, port string, ips []net.IPAddr) (net.Conn, error) {
	candidates := orderDialCandidates(ips)
	if len(candidates) == 0 {
		return nil, errors.New("no cached IP candidates")
	}
	if len(candidates) > dnsMaxDialCandidates {
		candidates = candidates[:dnsMaxDialCandidates]
	}

	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type dialResult struct {
		conn net.Conn
		err  error
	}
	results := make(chan dialResult, len(candidates))
	startDial := func(ip net.IP) {
		target := net.JoinHostPort(ip.String(), port)
		go func() {
			conn, err := d.dialer.DialContext(dialCtx, network, target)
			if err == nil && dialCtx.Err() != nil {
				_ = conn.Close()
				err = dialCtx.Err()
			}
			results <- dialResult{conn: conn, err: err}
		}()
	}

	started := 1
	finished := 0
	startDial(candidates[0])
	timer := time.NewTimer(dnsHappyEyeballsDelay)
	timerActive := true
	stopTimer := func() {
		if !timerActive {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerActive = false
	}
	resetTimer := func() {
		stopTimer()
		if started < len(candidates) {
			timer.Reset(dnsHappyEyeballsDelay)
			timerActive = true
		}
	}
	defer stopTimer()

	var lastErr error
	for finished < len(candidates) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			finished++
			if result.err == nil {
				cancel()
				return result.conn, nil
			}
			lastErr = result.err
			if started < len(candidates) && started == finished {
				startDial(candidates[started])
				started++
				resetTimer()
			}
		case <-timer.C:
			timerActive = false
			if started < len(candidates) {
				startDial(candidates[started])
				started++
			}
			resetTimer()
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("cached IP dial failed")
}

func orderDialCandidates(ips []net.IPAddr) []net.IP {
	var v6, v4 []net.IP
	var firstIsV4 *bool
	for _, ipAddr := range ips {
		ip := ipAddr.IP
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			if firstIsV4 == nil {
				v := true
				firstIsV4 = &v
			}
			v4 = append(v4, ip)
			continue
		}
		if firstIsV4 == nil {
			v := false
			firstIsV4 = &v
		}
		v6 = append(v6, ip)
	}
	out := make([]net.IP, 0, len(v4)+len(v6))
	if firstIsV4 != nil && *firstIsV4 {
		for len(v4) > 0 || len(v6) > 0 {
			if len(v4) > 0 {
				out = append(out, v4[0])
				v4 = v4[1:]
			}
			if len(v6) > 0 {
				out = append(out, v6[0])
				v6 = v6[1:]
			}
		}
		return out
	}
	for len(v6) > 0 || len(v4) > 0 {
		if len(v6) > 0 {
			out = append(out, v6[0])
			v6 = v6[1:]
		}
		if len(v4) > 0 {
			out = append(out, v4[0])
			v4 = v4[1:]
		}
	}
	return out
}

// lookup 返回 host 的解析结果，优先命中缓存。
func (d *cachedDialer) lookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	now := time.Now()

	d.mu.RLock()
	entry, ok := d.cache[host]
	d.mu.RUnlock()
	if ok && now.Before(entry.expiry) {
		return entry.ips, entry.err
	}

	// 回源解析。这里不做 singleflight：DNS 偶发并发回源代价可接受，换取实现简单可靠。
	ips, err := d.resolver.LookupIPAddr(ctx, host)

	d.mu.Lock()
	if err != nil {
		d.cache[host] = dnsCacheEntry{err: err, expiry: now.Add(dnsCacheNegativeTTL)}
	} else {
		d.cache[host] = dnsCacheEntry{ips: ips, expiry: now.Add(dnsCacheTTL)}
	}
	d.mu.Unlock()

	return ips, err
}

func (d *cachedDialer) invalidate(host string) {
	d.mu.Lock()
	delete(d.cache, host)
	d.mu.Unlock()
}

// runKeepWarm 周期性地为"已拨号过的直连上游"刷新 DNS 缓存，并为普通 TLS
// transport 做一次 TCP+TLS 拨号后立即关闭，从而把 TLS 会话票据填入共享
// ClientSessionCache。TLS fingerprint / uTLS 路径当前不复用此 cache。
//
// 严格只做网络层预热：不发任何 HTTP 请求、不带鉴权、不进选号/计费/冷却，
// 因此对所有业务功能零影响。
func (d *cachedDialer) runKeepWarm() {
	defer func() {
		// 后台 goroutine 兜底，绝不因 panic 影响主流程。
		_ = recover()
	}()

	ticker := time.NewTicker(keepWarmInterval)
	defer ticker.Stop()

	for range ticker.C {
		d.seenHosts.Range(func(key, _ any) bool {
			addr, ok := key.(string)
			if !ok || addr == "" {
				return true
			}
			d.warmTarget(addr)
			return true
		})
	}
}

// warmTarget 对单个 host:port 预热：刷新 DNS + 建立一次 TLS 连接后立即关闭。
func (d *cachedDialer) warmTarget(addr string) {
	defer func() { _ = recover() }()

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), keepWarmDialTimeout)
	defer cancel()

	// 刷新 DNS 缓存（强制回源，保持条目新鲜）。
	d.invalidate(host)

	// 建立底层 TCP（经 DNS 缓存）。
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return
	}

	// 在其上做一次标准 TLS 握手，把会话票据写入普通 TLS transport 的共享 cache。
	// 仅用于会话预热，握手完成即关闭，不复用此连接、不发数据。
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         host,
		ClientSessionCache: sharedUpstreamTLSSessionCache,
	})
	_ = tlsConn.HandshakeContext(ctx)
	_ = tlsConn.Close()
}

// Prewarm 主动解析并缓存一组域名，用于进程启动 / 后台保活时预热 DNS。
func (d *cachedDialer) Prewarm(ctx context.Context, hosts ...string) {
	for _, host := range hosts {
		host = canonicalDNSHost(host)
		if host == "" {
			continue
		}
		_, _ = d.lookup(ctx, host)
	}
}

// canonicalDNSHost 从可能带 scheme/port 的字符串里提取纯 host。
func canonicalDNSHost(raw string) string {
	if raw == "" {
		return ""
	}
	// 去掉 scheme。
	if i := indexOf(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	// 去掉 path。
	if i := indexByte(raw, '/'); i >= 0 {
		raw = raw[:i]
	}
	// 去掉 port。
	if h, _, err := net.SplitHostPort(raw); err == nil {
		return h
	}
	return raw
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
