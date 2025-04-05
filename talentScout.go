package LOLTalentScout

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/avast/retry-go"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"main.go/initialize"
	"main.go/lcu"
	"main.go/lcu/models"
	"main.go/scores"
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
}

func NewTalentScout() *TalentScout {
	ctx, cancel := context.WithCancel(context.Background())
	ts := &TalentScout{
		ctx:    ctx,
		cancel: cancel,
		mu:     &sync.Mutex{},
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
		fmt.Println("查询召唤师信息失败", zap.Error(err), zap.Any("summonerIDList", summonerIDList))
		return
	}
	for _, summoner := range summonerIDMapInfo {
		summoner := summoner
		summonerID := summoner.SummonerId
		g.Go(func() error {
			actScore, err := GetUserScore(summoner) //直接拿到评分
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
	//Wait等待errgroup中的所有协程完成，如果有一个失败则取消其他协程
	_ = g.Wait()
	slices.SortFunc(summonerScores, func(a, b *scores.UserScore) int {
		return cmp.Compare(b.Score, a.Score)
	})

	var MsgList []string
	allMsg := ""
	// 发送到选人界面
	for _, scoreInfo := range summonerScores {
		//判断是什么马
		horse := scores.Judge(scoreInfo.Score)
		//记录最近五场KDA
		currKDASb := strings.Builder{}
		for i := 0; i < 3 && i < len(scoreInfo.CurrKDA); i++ {
			currKDASb.WriteString(fmt.Sprintf("%d/%d/%d  ", scoreInfo.CurrKDA[i][0], scoreInfo.CurrKDA[i][1],
				scoreInfo.CurrKDA[i][2]))
		}
		currKDAMsg := currKDASb.String()

		if len(currKDAMsg) > 0 {
			currKDAMsg = currKDAMsg[:len(currKDAMsg)-1]
		}
		part := strings.Split(scoreInfo.SummonerName, "#")
		msg := fmt.Sprintf("%s\t[%s]-评分: %d 最近三场:%s", part[0], horse, int(scoreInfo.Score), currKDAMsg)
		MsgList = append(MsgList, msg)
		allMsg += msg + "\n"
	}
	fmt.Println(allMsg)
	SendMessage(MsgList, sessionId)

}

// CalcEnemyTeamScore 计算敌方分数
func (ts *TalentScout) CalcEnemyTeamScore() {
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
	selfTeamUsers, enemyTeamUsers := GetAllUsersFromSession(selfID, session)
	_ = selfTeamUsers
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
		fmt.Println("查询召唤师信息失败", zap.Error(err), zap.Any("summonerIDList", summonerIDList))
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
	// 根据所有用户的分数判断小代上等马中等马下等马
	allMsg := ""
	for _, score := range summonerScores {
		horse := scores.Judge(score.Score)
		currKDASb := strings.Builder{}
		for i := 0; i < 5 && i < len(score.CurrKDA); i++ {
			currKDASb.WriteString(fmt.Sprintf("%d/%d/%d  ", score.CurrKDA[i][0], score.CurrKDA[i][1],
				score.CurrKDA[i][2]))
		}
		currKDAMsg := currKDASb.String()
		part := strings.Split(score.SummonerName, "#")
		msg := fmt.Sprintf("%s\t[%s]-综合评分: %d 最近三场:%s", part[0], horse, int(score.Score), currKDAMsg)
		allMsg += msg + "\n"
	}
	fmt.Println(allMsg)
}

// 根据客户端推送的信息，实时更新客户端状态
func (ts *TalentScout) onGameFlowUpdate(gameFlow string) {
	fmt.Println("切换状态:" + gameFlow)
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

	//其他状态
	default:
		ts.updateGameState(GameStateOther)
	}

}

// 和客户端之间推送信息的格式
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
	//向客户端发送了[5, "OnJsonApiEvent"]，可能是有某种协商？目前不太清楚
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

func (ts *TalentScout) Run() {
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
