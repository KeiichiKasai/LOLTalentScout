package initialize

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"github.com/gorilla/websocket"
	"net"
	"net/http"
	"net/url"
)

func InitWSS(port int, token string) (*websocket.Conn, error) {
	//定义了一个自定义的网络拨号器（dialer），用于通过TCP协议连接到指定的服务器地址。
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	//通过一系列本地IP地址来尝试与目标服务器建立连接，如果第一次尝试失败，则会使用下一个本地IP地址进行重试，最多尝试10次
	dialer.NetDialContext = func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
		localAddr := &net.TCPAddr{IP: []byte{127, 0, 0, 100}}
		//ResolveTCPAddr把服务器解析成*TCPAddr
		serverAddr, err := net.ResolveTCPAddr(network, addr)
		if err != nil {
			return nil, err
		}
		localAddr.Port = serverAddr.Port
		for i := 0; i < 10; i++ {
			localAddr.IP[3] += (byte)(i)
			conn, err = net.DialTCP("tcp", localAddr, serverAddr)
			if err == nil {
				break
			}
		}
		return conn, err
	}
	//rawUrl是本地游戏LCU客户端的地址，这里是和客户端保持一个长连接
	rawUrl := fmt.Sprintf("wss://127.0.0.1:%d/", port)
	header := http.Header{}
	authSecret := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("riot:%s", token)))
	header.Set("Authorization", "Basic "+authSecret)
	u, _ := url.Parse(rawUrl)
	c, _, err := dialer.Dial(u.String(), header)
	return c, err
}
