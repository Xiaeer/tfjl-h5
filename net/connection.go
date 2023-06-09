package net

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"tfjl-h5/configs"
	"tfjl-h5/iface"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type Connection struct {
	//当前链接所属Server
	Server iface.IServer
	Conn   *websocket.Conn
	connId uint64
	//inChan            chan *Message
	outChan           chan *Message
	isClosed          bool
	closeChan         chan byte
	rooms             map[string]bool
	MsgHandle         iface.IMsgHandle
	lastHeartBeatTime time.Time
	//链接属性
	property map[string]interface{}
	//保护链接属性修改的锁
	propertyLock sync.RWMutex

	mutex sync.Mutex
}

// 初始化链接服务
func NewConnection(server iface.IServer, wsSocket *websocket.Conn, connId uint64, msgHandler iface.IMsgHandle) *Connection {
	c := &Connection{
		Server:    server,
		Conn:      wsSocket,
		connId:    connId,
		MsgHandle: msgHandler,
		//inChan:            	make(chan *Message, configs.GConf.InChanSize),
		outChan:           make(chan *Message, configs.GConf.OutChanSize),
		closeChan:         make(chan byte),
		lastHeartBeatTime: time.Now(),
		rooms:             make(map[string]bool),
	}
	c.Server.GetConnMgr().Add(c)
	return c
}

// 开始
func (c *Connection) Start() {
	go c.readLoop()
	go c.writeLoop()
}

// 关闭连接
func (c *Connection) Close() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.Conn.Close()
	if !c.isClosed {
		c.isClosed = true
		close(c.closeChan)
	}
	c.Server.GetConnMgr().Remove(c)
}

// 获取链接对象
func (c *Connection) GetConnection() *websocket.Conn {
	return c.Conn
}

// 获取链接ID
func (c *Connection) GetConnID() uint64 {
	return c.connId
}

// 获取远程客户端地址信息
func (c *Connection) RemoteAddr() net.Addr {
	return c.Conn.RemoteAddr()
}

// 设置链接属性
func (c *Connection) SetProperty(key string, value interface{}) {
	c.propertyLock.Lock()
	defer c.propertyLock.Unlock()
	if c.property == nil {
		c.property = make(map[string]interface{})
	}

	c.property[key] = value
}

// 获取链接属性
func (c *Connection) GetProperty(key string) (interface{}, error) {
	c.propertyLock.RLock()
	defer c.propertyLock.RUnlock()

	if value, ok := c.property[key]; ok {
		return value, nil
	} else {
		return nil, errors.New("no property found")
	}
}

// 移除链接属性
func (c *Connection) RemoveProperty(key string) {
	c.propertyLock.Lock()
	defer c.propertyLock.Unlock()

	delete(c.property, key)
}

// 读websocket
func (c *Connection) readLoop() {
	var (
		msgType int
		msgData []byte
		err     error
	)

	for {
		if msgType, msgData, err = c.Conn.ReadMessage(); err != nil {
			goto ERR
		}

		if msgType != websocket.BinaryMessage || uint32(len(msgData)) < 8 {
			logrus.Error("not BinaryMessage or message length error")
			goto ERR
		}

		buffer := bytes.NewBuffer(msgData)
		var length uint32
		err = binary.Read(buffer, binary.LittleEndian, &length)
		if err != nil || uint32(len(msgData)) != length {
			logrus.Error(err, " or message length error")
			goto ERR
		}
		var protocolNum uint32
		err = binary.Read(buffer, binary.LittleEndian, &protocolNum)
		if err != nil {
			logrus.Error(err)
			return
		}

		message := NewMsg(msgType, protocolNum, buffer.Bytes())
		//得到当前客户端请求的Request数据
		req := Request{
			conn: c,
			msg:  message,
		}
		c.KeepAlive()
		if configs.GConf.WorkerPoolSize > 0 {
			//已经启动工作池机制，将消息交给Worker处理
			c.MsgHandle.SendMsgToTaskQueue(&req)
		} else {
			//从绑定好的消息和对应的处理方法中执行对应的Handle方法
			go c.MsgHandle.DoMsgHandler(&req)
		}
	}

ERR:
	c.Close()
}

// 写websocket
func (c *Connection) writeLoop() {
	var (
		err error
	)
	for {
		select {
		case message := <-c.outChan:
			var length uint32 = uint32(8 + len(message.GetData()))
			var buffer bytes.Buffer
			err = binary.Write(&buffer, binary.LittleEndian, length)
			if err != nil {
				logrus.Error(err)
				goto ERR
			}
			err = binary.Write(&buffer, binary.LittleEndian, message.GetMsgID())
			if err != nil {
				logrus.Error(err)
				goto ERR
			}
			err = binary.Write(&buffer, binary.LittleEndian, message.GetData())
			if err != nil {
				logrus.Error(err)
				goto ERR
			}
			if err = c.Conn.WriteMessage(message.MsgType, buffer.Bytes()); err != nil {
				fmt.Println(err)
				goto ERR
			}
			c.KeepAlive()
		case <-c.closeChan:
			goto CLOSED
		}
	}
ERR:
	c.Close()
CLOSED:
}

// 发送消息
func (c *Connection) SendMessage(msgType int, msgID uint32, msgData []byte) (err error) {
	message := NewMsg(msgType, msgID, msgData)
	select {
	case c.outChan <- message:
	case <-c.closeChan:
		err = errors.New("ERR_CONNECTION_LOSS")
	default: // 写操作不会阻塞, 因为channel已经预留给websocket一定的缓冲空间
		err = errors.New("ERR_SEND_MESSAGE_FULL")
	}
	return
}

// 读取消息
/*
func (c *Connection) ReadMessage() (message *Message,err error) {
	select {
	case message = <-c.inChan:
	case <-c.closeChan:
		err = errors.New("ERR_CONNECTION_LOSS")
	}
	return
}
**/

// 定时检测心跳包
func (c *Connection) heartBeatChecker() {
	var (
		timer *time.Timer
	)

	timer = time.NewTimer(time.Duration(configs.GConf.HeartBeatTime) * time.Second)

	for {
		select {
		case <-timer.C:
			if !c.IsAlive() {
				c.Close()
			}

			timer.Reset(time.Duration(configs.GConf.HeartBeatTime) * time.Second)
		case <-c.closeChan:
			timer.Stop()

		}
	}
}

// 检测心跳
func (c *Connection) IsAlive() bool {
	var (
		now = time.Now()
	)
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.isClosed || now.Sub(c.lastHeartBeatTime) > time.Duration(configs.GConf.HeartBeatTime)*time.Second {
		return false
	}
	return true

}

// 更新心跳
func (c *Connection) KeepAlive() {
	var (
		now = time.Now()
	)
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.lastHeartBeatTime = now
}
