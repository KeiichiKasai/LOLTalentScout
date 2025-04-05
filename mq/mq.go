package mq

import (
	"fmt"
	amqp "github.com/rabbitmq/amqp091-go"
	"main.go/lcu"
	"strings"
	"time"
)

func InitMQ() *amqp.Connection {
	conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	if err != nil {
		fmt.Println("消息队列连接失败")
		return nil
	}
	return conn
}
func Listen(conn *amqp.Connection) {
	//建立与MQ的轻量级连接
	ch, err := conn.Channel()
	if err != nil {
		fmt.Println("建立通道失败")
		return
	}
	defer ch.Close()

	q, err := ch.QueueDeclare(
		"msg_queue", // 队列名称
		false,       // 持久化
		false,       // 自动删除
		false,       // 独占的
		false,       // 无等待
		nil,         // 参数
	)
	if err != nil {
		fmt.Println("创建队列失败")
		return
	}
	// 消费消息
	msgs, err := ch.Consume(
		q.Name, // 队列名称
		"",     // 消费者标签，空字符串表示自动生成
		true,   // 自动确认
		false,  // 独占
		false,  // 不本地
		false,  // 等待
		nil,    // 参数
	)
	if err != nil {
		fmt.Println("消费消息失败")
		return
	}
	//开始监听
	//应该会传入的消息格式 "召唤师名称\t[上等马]-评分: 100 最近三场: 0/0/0 0/0/0 0/0/0|conversationID"
	for {
		for d := range msgs {
			str := string(d.Body)
			part := strings.Split(str, "|")
			msg, conversationID := part[0], part[1]
			err = lcu.SendConversationMsg(msg, conversationID)
			if err != nil {
				fmt.Println("此消息发送失败:", msg)
				continue
			}
			time.Sleep(2 * time.Second) //连续发送会导致LOL禁言,所以暂停2秒
		}
	}
}
func Produce(conn *amqp.Connection, msgs []string) {
	ch, err := conn.Channel()
	if err != nil {
		fmt.Println("建立通道失败")
		return
	}
	defer ch.Close()

	q, _ := ch.QueueDeclare(
		"msg_queue", // 队列名称
		false,       // 持久化
		false,       // 自动删除
		false,       // 独占的
		false,       // 无等待
		nil,         // 参数
	)
	for _, msg := range msgs {
		err = ch.Publish(
			"",     // 交换机exchange的名字，空字符串表示使用默认交换机
			q.Name, // routing key
			false,
			false,
			amqp.Publishing{
				ContentType: "text/plain",
				Body:        []byte(msg),
			})
		if err != nil {
			fmt.Println("此消息未传到rabbitMQ:", msg)
		}
	}
}
