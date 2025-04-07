package LOLTalentScout

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/avast/retry-go"
	"github.com/gorilla/websocket"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"main.go/initialize"
	"main.go/lcu"
	"main.go/lcu/models"
	"main.go/mq"
	"main.go/scores"
	"main.go/utils"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// TalentScout 一个集成控制中心
type TalentScout struct {
	ctx          context.Context
	httpSrv      *http.Server
	lcuPort      int
	lcuToken     string
	lcuActive    bool
	currSummoner *models.CurrSummoner
	cancel       func()
	mu           *sync.Mutex
	GameState    GameState
	MqConn       *amqp.Connection
}

func NewTalentScout() *TalentScout {
	ctx, cancel := context.WithCancel(context.Background())
	ts := &TalentScout{
		ctx:    ctx,
		cancel: cancel,
		mu:     &sync.Mutex{},
		//MqConn: mq.InitMQ(), 启用消息队列
	}
	return ts
}

// updateGameState 更新客户端状态
func (ts *TalentScout) updateGameState(state GameState) {
	ts.mu.Lock()
	ts.GameState = state
	ts.mu.Unlock()
}

// CalcTeamScore 计算队伍成员得分
func (ts *TalentScout) CalcTeamScore() {
	var summonerIDList []int64
	var sessionId string
	for i := 0; i < 3; i++ {
		//重复3次是为了防止有老年机半天进不来聊天室
		time.Sleep(time.Second)
		// 获取队伍所有用户信息
		sessionId, summonerIDList, _ = getTeamUsers()
		if len(summonerIDList) != 5 {
			continue
		}
	}
	if len(summonerIDList) == 0 {
		return
	}
	fmt.Println("队伍人员列表:", summonerIDList)
	// 查询所有用户的信息并计算得分
	g := errgroup.Group{}
	summonerScores := make([]*scores.UserScore, 0, 5)
	mu := sync.Mutex{}
	summonerIDMapInfo, err := listSummoner(summonerIDList)
	if err != nil {
		fmt.Println("查询召唤师信息失败")
		return
	}
	for _, summoner := range summonerIDMapInfo {
		summoner := summoner
		summonerID := summoner.SummonerId
		g.Go(func() error {
			actScore, err := GetUserScore(summoner) //直接拿到评分
			if err != nil {
				fmt.Println("计算用户", summonerID, "得分失败")
				return nil
			}
			mu.Lock()
			summonerScores = append(summonerScores, actScore)
			mu.Unlock()
			return nil
		})
	}
	//Wait等待errgroup中的所有协程完成，如果有一个失败则取消其他协程
	_ = g.Wait()
	slices.SortFunc(summonerScores, func(a, b *scores.UserScore) int {
		return cmp.Compare(b.Score, a.Score)
	})

	var MsgList []string
	allMsg := ""

	for _, scoreInfo := range summonerScores {
		//判断是什么马
		horse := scores.Judge(scoreInfo.Score)
		//限制名字长度
		name := utils.TruncateString(scoreInfo.SummonerName, 5)
		//大乱斗玩家实力不详，特殊处理
		if scoreInfo.IsARAM {
			msg := fmt.Sprintf("%s\t[%s]-评分: %d 【大乱斗玩家,实力不详,遇弱则强,遇强则弱】", name, horse, int(scoreInfo.Score))
			MsgList = append(MsgList, msg)
			allMsg += msg + "\n"
			continue
		}
		//峡谷玩家则展示KDA
		//记录最近五场KDA
		currKDASb := strings.Builder{}
		sevenKDASb := strings.Builder{}
		for i := 0; i < 7 && i < len(scoreInfo.CurrKDA); i++ {
			//客户端只发送三局
			if i < 3 {
				currKDASb.WriteString(fmt.Sprintf("%d/%d/%d  ", scoreInfo.CurrKDA[i][0], scoreInfo.CurrKDA[i][1],
					scoreInfo.CurrKDA[i][2]))
			}
			//命令行可以看到七局
			sevenKDASb.WriteString(fmt.Sprintf("%d/%d/%d  ", scoreInfo.CurrKDA[i][0], scoreInfo.CurrKDA[i][1],
				scoreInfo.CurrKDA[i][2]))
		}
		currKDAMsg := currKDASb.String()
		sevenKDAMsg := sevenKDASb.String()

		if len(currKDAMsg) > 0 {
			currKDAMsg = currKDAMsg[:len(currKDAMsg)-1]
		}
		if len(sevenKDAMsg) > 0 {
			sevenKDAMsg = sevenKDAMsg[:len(sevenKDAMsg)-1]
		}

		//发送给客户端的数据
		msg := fmt.Sprintf("%s\t[%s]-评分: %d 最近三场:%s", name, horse, int(scoreInfo.Score), currKDAMsg)
		MsgList = append(MsgList, msg)
		//发送到命令行的数据
		allMsg += fmt.Sprintf("%s\t[%s]-评分: %d 最近七场:%s\n", name, horse, int(scoreInfo.Score), sevenKDAMsg)
	}
	fmt.Println(allMsg)
	//ts.PushMsgToMq(MsgList, sessionId)
	SendMessage(MsgList, sessionId)
}

// CalcEnemyTeamScore 计算敌方分数
func (ts *TalentScout) CalcEnemyTeamScore() {
	//游戏开始后拿到sessionID
	session, err := lcu.QueryGameFlowSession()
	if err != nil {
		return
	}
	if session.Phase != models.GameFlowInProgress {
		return
	}
	if ts.currSummoner == nil {
		return
	}
	selfID := ts.currSummoner.SummonerId
	//通过sessionID拿到所有人信息
	_, enemyTeamUsers := GetAllUsersFromSession(selfID, session)
	summonerIDList := enemyTeamUsers

	fmt.Println("敌方队伍人员列表:", summonerIDList)
	if len(summonerIDList) == 0 {
		return
	}
	// 查询所有用户的信息并计算得分
	g := errgroup.Group{}
	summonerScores := make([]*scores.UserScore, 0, 5)
	mu := sync.Mutex{}
	summonerIDMapInfo, err := listSummoner(summonerIDList)
	if err != nil {
		fmt.Println("查询敌方召唤师信息失败")
		return
	}
	for _, summoner := range summonerIDMapInfo {
		summoner := summoner
		summonerID := summoner.SummonerId
		g.Go(func() error {
			actScore, err := GetUserScore(summoner)
			if err != nil {
				fmt.Println("计算用户得分失败", zap.Error(err), zap.Int64("summonerID", summonerID))
				return nil
			}
			mu.Lock()
			summonerScores = append(summonerScores, actScore)
			mu.Unlock()
			return nil
		})
	}

	_ = g.Wait()
	if len(summonerScores) > 0 {
		fmt.Println("敌方用户详情:")
	}
	slices.SortFunc(summonerScores, func(a, b *scores.UserScore) int {
		return cmp.Compare(b.Score, a.Score)
	})
	// 根据所有用户的分数判断实力
	allMsg := ""
	for _, scoreInfo := range summonerScores {
		name := utils.TruncateString(scoreInfo.SummonerName, 5)
		horse := scores.Judge(scoreInfo.Score)
		//大乱斗玩家特殊对待
		if scoreInfo.IsARAM {
			msg := fmt.Sprintf("%s\t[%s]-评分: %d 【大乱斗玩家,实力不详,遇弱则强,遇强则弱】", name, horse, int(scoreInfo.Score))
			allMsg += msg + "\n"
			continue
		}
		currKDASb := strings.Builder{}
		for i := 0; i < 7 && i < len(scoreInfo.CurrKDA); i++ {
			currKDASb.WriteString(fmt.Sprintf("%d/%d/%d  ", scoreInfo.CurrKDA[i][0], scoreInfo.CurrKDA[i][1],
				scoreInfo.CurrKDA[i][2]))
		}
		currKDAMsg := currKDASb.String()
		msg := fmt.Sprintf("%s\t[%s]-综合评分: %d 最近七场:%s", name, horse, int(scoreInfo.Score), currKDAMsg)
		allMsg += msg + "\n"
	}
	fmt.Println(allMsg)
}

// PushMsgToMq 把消息发送给MQ
func (ts *TalentScout) PushMsgToMq(msgList []string, sessionId string) {
	m := "LOL伯乐正在寻找千里马...|" + sessionId
	msgs := []string{m}
	for _, msg := range msgList {
		msgs = append(msgs, msg+"|"+sessionId)
	}
	mq.Produce(ts.MqConn, msgs)
}

func (ts *TalentScout) AcceptGame() {
	err := acceptGame()
	if err != nil {
		return
	}
	fmt.Println("已自动接受对局,不允许临阵脱逃噢")
}

// onGameFlowUpdate 根据客户端推送的信息，实时更新客户端状态
func (ts *TalentScout) onGameFlowUpdate(gameFlow string) {
	fmt.Println("当前状态:" + gameFlow)
	switch gameFlow {

	// 英雄选择状态
	case string(models.GameFlowChampionSelect):
		fmt.Println("进入英雄选择阶段,正在计算用户分数")
		ts.updateGameState(GameStateChampSelect)
		go ts.CalcTeamScore() //开个协程去计算队友分数
	// 大厅等待状态
	case string(models.GameFlowNone):
		ts.updateGameState(GameStateNone)

	// 匹配状态
	case string(models.GameFlowMatchmaking): //匹配中
		ts.updateGameState(GameStateMatchmaking)

	// 游戏加载状态
	case string(models.GameFlowInProgress):
		fmt.Println("已进入游戏,正在计算敌方分数")
		ts.updateGameState(GameStateInGame)
		go ts.CalcEnemyTeamScore() //开个协程去计算敌方分数

	//对局确认状态
	case string(models.GameFlowReadyCheck):
		ts.updateGameState(GameStateReadyCheck)
		go ts.AcceptGame() //自动接受对局
	//其他状态
	default:
		ts.updateGameState(GameStateOther)
	}

}

// wsMsg 和客户端之间推送信息的格式
type wsMsg struct {
	Data      interface{} `json:"data"`
	EventType string      `json:"event_type"`
	Uri       string      `json:"uri"`
}

// InitGameFlowMonitor 监控客户端
func (ts *TalentScout) InitGameFlowMonitor(port int, token string) error {
	//建立WSS协议连接
	c, err := initialize.InitWSS(port, token)
	if err != nil {
		return err
	}
	fmt.Println("--------[成功连接到LOL客户端]--------")
	defer func() {
		_ = c.Close()
	}()
	//使用了 retry 包来尝试多次调用 lcu.GetCurrSummoner() 函数，目的是获取当前召唤师（currSummoner）的信息
	err = retry.Do(func() error {
		currSummoner, err := lcu.GetCurrSummoner()
		if err == nil {
			ts.currSummoner = currSummoner
		}
		return err
	}, retry.Attempts(5), retry.Delay(time.Second))
	if err != nil {
		return errors.New("获取当前召唤师信息失败:" + err.Error())
	}
	//如果获取到了召唤师信息则LCU客户端连接成功，把状态设置为活跃
	ts.lcuActive = true
	//向客户端发送[5, "OnJsonApiEvent"],请求交互信息
	_ = c.WriteMessage(websocket.TextMessage, []byte("[5, \"OnJsonApiEvent\"]"))
	for {
		msgType, message, err := c.ReadMessage()
		if err != nil {
			fmt.Println("lol事件监控读取消息失败", zap.Error(err))
			return err
		}
		msg := &wsMsg{}
		if msgType != websocket.TextMessage || len(message) < onJsonApiEventPrefixLen+1 {
			continue
		}
		//去掉message前缀和尾部然后反序列化到msg上
		_ = json.Unmarshal(message[onJsonApiEventPrefixLen:len(message)-1], msg)
		// log.Println("ws evt: ", msg.Uri)
		//根据收到信息的uri来进行相应操作
		switch msg.Uri {
		case string(gameFlowChangedEvt):
			gameFlow, ok := msg.Data.(string)
			if !ok {
				continue
			}
			ts.onGameFlowUpdate(gameFlow)
		case string(champSelectUpdateSessionEvt): //选择英雄阶段信息（皮肤，召唤师技能，骰子等）
			//TODO 一键ban/pick
		default:
		}
	}
}

// Run 启动TalentScout
func (ts *TalentScout) Run() {
	//开启监听
	//go mq.Listen(ts.MqConn)
	//重连次数
	connection := 1
	for {
		//如果没有连上客户端
		if !ts.lcuActive {
			//获取LCU客户端token和port
			token, port := initialize.NewCertificate()
			//先持久化一个客户端连接
			lcu.InitCli(port, token)
			//基于wss与客户端建立一个实时通讯
			err := ts.InitGameFlowMonitor(port, token)
			if err != nil {
				fmt.Println(fmt.Sprintf("未检测到LOL客户端，正在尝试重连......[重连次数:%d]", connection))
				connection++
				time.Sleep(5 * time.Second)
			}
		}
	}
}
