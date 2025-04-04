package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/avast/retry-go"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/windows"
	"main.go/lcu"
	"main.go/lcu/models"
	"main.go/score"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// 前期连接
const (
	lolUxProcessName                  = "LeagueClientUx.exe"
	ProcessCommandLineInformation     = 60
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
)

var (
	lolCommandlineReg             = regexp.MustCompile(`--remoting-auth-token=(.+?)" ".*?--app-port=(\d+)"`)
	modntdll                      = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryInformationProcess = modntdll.NewProc("NtQueryInformationProcess")
)

type UNICODE_STRING struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

func getProcessPidByName(name string) ([]int, error) {
	cmd := exec.Command("wmic", "process", "where", fmt.Sprintf("name like '%%%s%%'", name), "get", "processid")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	// 将输出按行分割
	lines := strings.Split(string(output), "\n")
	var pids []int

	// 处理每行输出
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 0 {
			// 转换为数字并添加到结果中
			pid, err := strconv.Atoi(trimmed)
			if err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids, nil
}

func GetProcessCommandLine(pid uint32) (string, error) {
	// Open the process with PROCESS_QUERY_LIMITED_INFORMATION
	handle, err := windows.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", fmt.Errorf("failed to open process: %v", err)
	}
	defer windows.CloseHandle(handle)

	// Query the buffer length for the command line information
	var bufLen uint32
	r1, _, err := procNtQueryInformationProcess.Call(
		uintptr(handle),
		uintptr(ProcessCommandLineInformation),
		0,
		0,
		uintptr(unsafe.Pointer(&bufLen)),
	)

	// Allocate buffer to hold command line information
	buffer := make([]byte, bufLen)
	r1, _, err = procNtQueryInformationProcess.Call(
		uintptr(handle),
		uintptr(ProcessCommandLineInformation),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(bufLen),
		uintptr(unsafe.Pointer(&bufLen)),
	)
	if r1 != 0 {
		return "", fmt.Errorf("NtQueryInformationProcess failed, error code: %v", err)
	}

	// Check if the buffer length is valid and non-zero
	if bufLen == 0 {
		return "", fmt.Errorf("No command line found for process %d", pid)
	}

	// Parse the buffer into a UNICODE_STRING
	ucs := (*UNICODE_STRING)(unsafe.Pointer(&buffer[0]))
	cmdLine := windows.UTF16ToString((*[1 << 20]uint16)(unsafe.Pointer(ucs.Buffer))[:ucs.Length/2])

	return cmdLine, nil
}

// GameState 客户端状态
type GameState string

const (
	GameStateNone        GameState = "none"
	GameStateChampSelect GameState = "champSelect"
	GameStateReadyCheck  GameState = "ReadyCheck"
	GameStateInGame      GameState = "inGame"
	GameStateOther       GameState = "other"
	GameStateMatchmaking GameState = "Matchmaking"
)

// ws需要
type lcuWsEvt string

const (
	onJsonApiEventPrefixLen              = len(`[8,"OnJsonApiEvent",`)
	gameFlowChangedEvt          lcuWsEvt = "/lol-gameflow/v1/gameflow-phase"
	champSelectUpdateSessionEvt lcuWsEvt = "/lol-champ-select/v1/session"
)

// 和客户端之间推送信息的格式
type wsMsg struct {
	Data      interface{} `json:"data"`
	EventType string      `json:"event_type"`
	Uri       string      `json:"uri"`
}

// TalentScout 一个集成控制中心
type TalentScout struct {
	ctx          context.Context
	httpSrv      *http.Server
	lcuPort      int
	lcuToken     string
	lcuActive    bool
	currSummoner *lcu.CurrSummoner
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

const (
	defaultScore       = 100 // 默认分数
	minGameDurationSec = 15 * 60
)

// 更新客户端状态
func (ts *TalentScout) updateGameState(state GameState) {
	ts.mu.Lock()
	ts.GameState = state
	ts.mu.Unlock()
}

// getTeamUsers 拿会话id和队伍用户id列表
func getTeamUsers() (string, []int64, error) {
	conversationID, err := lcu.GetCurrConversationID()
	if err != nil {
		return "", nil, err
	}
	msgList, err := lcu.ListConversationMsg(conversationID)
	if err != nil {
		return "", nil, err
	}
	//从会话组信息拿到我方五人ID
	summonerIDList := getSummonerIDListFromConversationMsgList(msgList)
	return conversationID, summonerIDList, nil
}

// listSummoner 通过用户id列表拿到所有个人信息
func listSummoner(summonerIDList []int64) (map[int64]*lcu.Summoner, error) {
	list, err := lcu.ListSummoner(summonerIDList)
	if err != nil {
		return nil, err
	}
	res := make(map[int64]*lcu.Summoner, len(summonerIDList))
	for _, summoner := range list {
		summoner := summoner
		res[summoner.SummonerId] = &summoner
	}
	return res, nil
}

// getSummonerIDListFromConversationMsgList 从聊天室拿到五人id
func getSummonerIDListFromConversationMsgList(msgList []lcu.ConversationMsg) []int64 {
	summonerIDList := make([]int64, 0, 5)
	for _, msg := range msgList {
		//初始进入聊天室会发送进入信息，从中获取
		if msg.Type == lcu.ConversationMsgTypeSystem && msg.Body == lcu.JoinedRoomMsg && msg.FromSummonerId > 0 {
			summonerIDList = append(summonerIDList, msg.FromSummonerId)
		}
	}
	return summonerIDList
}

// listGameHistory 根据用户puuid拿到历史战绩
func listGameHistory(puuid string) ([]lcu.GameInfo, error) {
	limit := 20
	fmtList := make([]lcu.GameInfo, 0, limit)
	resp, err := lcu.ListGamesByPUUID(puuid, 0, limit)
	if err != nil {
		fmt.Println("查询用户战绩失败", zap.Error(err), zap.String("puuid", puuid))
		return nil, err
	}
	for _, gameItem := range resp.Games.Games {
		if gameItem.QueueId != models.NormalQueueID &&
			gameItem.QueueId != models.RankSoleQueueID &&
			gameItem.QueueId != models.ARAMQueueID &&
			gameItem.QueueId != models.RankFlexQueueID {
			continue
		}
		if gameItem.GameDuration < minGameDurationSec {
			continue
		}
		fmtList = append(fmtList, gameItem)
	}
	gameCount := len(fmtList)
	//逆序返回
	for i := 0; i < gameCount/2; i++ {
		fmtList[i], fmtList[gameCount-1-i] = fmtList[gameCount-1-i], fmtList[i]
	}
	return fmtList, nil
}

// GetUserScore 从个人信息得到用户最近评分
func GetUserScore(summoner *lcu.Summoner) (*score.UserScore, error) {

	summonerID := summoner.SummonerId
	userScoreInfo := &score.UserScore{
		SummonerID: summonerID,
		Score:      defaultScore,
	}
	userScoreInfo.SummonerName = fmt.Sprintf("%s#%s", summoner.GameName, summoner.TagLine)
	// 获取最近20场战绩列表
	gameList, err := listGameHistory(summoner.Puuid)
	if err != nil {
		fmt.Println("获取用户战绩失败", zap.Error(err), zap.Int64("id", summonerID))
		return userScoreInfo, nil
	}
	// 获取每一局战绩KDA
	g := errgroup.Group{}
	gameSummaryList := make([]lcu.GameSummary, 0, len(gameList))
	mu := sync.Mutex{}
	currKDAList := make([][3]int, len(gameList))
	for i, info := range gameList {
		info := info
		currKDAList[len(gameList)-i-1] = [3]int{
			info.Participants[0].Stats.Kills,
			info.Participants[0].Stats.Deaths,
			info.Participants[0].Stats.Assists,
		}
		//从20对局信息里拿到我们需要的信息反序列化到lcu.GameSummary加入到gameSummaryList
		g.Go(func() error {
			var gameSummary *lcu.GameSummary
			err = retry.Do(func() error {
				var tmpErr error
				gameSummary, tmpErr = lcu.QueryGameSummary(info.GameId)
				return tmpErr
			}, retry.Delay(time.Millisecond*10), retry.Attempts(5))
			if err != nil {
				fmt.Println("获取游戏对局详细信息失败", zap.Error(err), zap.Int64("id", info.GameId))
				return nil
			}
			mu.Lock()
			gameSummaryList = append(gameSummaryList, *gameSummary)
			mu.Unlock()
			return nil
		})
	}
	userScoreInfo.CurrKDA = currKDAList
	err = g.Wait() //等待所有协程退出
	if err != nil {
		fmt.Println("获取用户详细战绩失败", zap.Error(err), zap.Int64("id", summonerID))
		return userScoreInfo, nil
	}
	//最近20场总分
	var totalScore float64 = 0
	totalGameCount := 0
	type gameScoreWithWeight struct {
		score       float64
		isCurrTimes bool //是否最近五小时
	}
	// gameWeightScoreList := make([]gameScoreWithWeight, 0, len(gameSummaryList))
	nowTime := time.Now()
	// 根据权重分析每一局战绩计算得分
	currTimeScoreList := make([]float64, 0, 10)  //最近五小时分数列表
	otherGameScoreList := make([]float64, 0, 10) //其他时间分数列表
	for _, gameSummary := range gameSummaryList {
		//得到用户该局分数
		gameScore, err := score.CalcUserGameScore(summonerID, gameSummary)
		if err != nil {
			fmt.Println("游戏战绩计算用户得分失败", zap.Error(err), zap.Int64("summonerID", summonerID),
				zap.Int64("gameID", gameSummary.GameId))
			return userScoreInfo, nil
		}
		weightScoreItem := gameScoreWithWeight{
			score:       gameScore.Value(),
			isCurrTimes: nowTime.Before(gameSummary.GameCreationDate.Add(time.Hour * 5)),
		}
		if weightScoreItem.isCurrTimes {
			currTimeScoreList = append(currTimeScoreList, gameScore.Value())
		} else {
			otherGameScoreList = append(otherGameScoreList, gameScore.Value())
		}
		totalGameCount++
		totalScore += gameScore.Value()
		// log.Printf("game: %d,得分: %.2f\n", gameSummary.GameId, gameScore)
	}

	totalGameScore := 0.0      //总得分
	totalTimeScore := 0.0      //最近五小时得分
	avgTimeScore := 0.0        //最近五小时平均得分
	totalOtherGameScore := 0.0 //其他时间得分
	avgOtherGameScore := 0.0   //其他时间平均得分

	for _, score := range currTimeScoreList {
		totalTimeScore += score
		totalGameScore += score
	}
	for _, score := range otherGameScoreList {
		totalOtherGameScore += score
		totalGameScore += score
	}
	if totalTimeScore > 0 {
		avgTimeScore = totalTimeScore / float64(len(currTimeScoreList))
	}
	if totalOtherGameScore > 0 {
		avgOtherGameScore = totalOtherGameScore / float64(len(otherGameScoreList))
	}
	totalGameAvgScore := 0.0
	if totalGameCount > 0 {
		totalGameAvgScore = totalGameScore / float64(totalGameCount)
	}
	weightTotalScore := 0.0
	// 最近五小时
	{
		if len(currTimeScoreList) == 0 {
			//如果最近没打，就把近二十场平均得分当作最近五小时平均得分
			weightTotalScore += .8 * totalGameAvgScore
		} else {
			weightTotalScore += .8 * avgTimeScore
		}
	}
	// 其他时间
	{
		if len(otherGameScoreList) == 0 {
			//如果五小时玩了20把，就把近二十场平均得分当作其他时间平均得分，其实不可能五小时打20把
			weightTotalScore += .2 * totalGameAvgScore
		} else {
			weightTotalScore += .2 * avgOtherGameScore
		}
	}
	// 计算平均值返回
	// userScoreInfo.Score = totalScore / float64(totalGameCount)
	if len(gameSummaryList) == 0 {
		weightTotalScore = defaultScore
	}
	userScoreInfo.Score = weightTotalScore
	return userScoreInfo, nil
}

// CalcTeamScore 计算我方分数
func (ts *TalentScout) CalcTeamScore() {
	var summonerIDList []int64
	for i := 0; i < 3; i++ {
		//重复3次是为了防止有老年机半天进不来聊天室
		time.Sleep(time.Second)
		// 获取队伍所有用户信息
		_, summonerIDList, _ = getTeamUsers()
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
	summonerScores := make([]*score.UserScore, 0, 5)
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
	slices.SortFunc(summonerScores, func(a, b *score.UserScore) int {
		return cmp.Compare(b.Score, a.Score)
	})

	allMsg := ""
	// 发送到选人界面
	for _, scoreInfo := range summonerScores {
		//判断是什么马
		horse := score.Judge(scoreInfo.Score)
		//记录最近五场KDA
		currKDASb := strings.Builder{}
		for i := 0; i < 5 && i < len(scoreInfo.CurrKDA); i++ {
			currKDASb.WriteString(fmt.Sprintf("%d/%d/%d  ", scoreInfo.CurrKDA[i][0], scoreInfo.CurrKDA[i][1],
				scoreInfo.CurrKDA[i][2]))
		}
		currKDAMsg := currKDASb.String()

		if len(currKDAMsg) > 0 {
			currKDAMsg = currKDAMsg[:len(currKDAMsg)-1]
		}
		msg := fmt.Sprintf("%s(%d): %s %s", horse, int(scoreInfo.Score), scoreInfo.SummonerName,
			currKDAMsg)
		allMsg += msg + "\n"
	}
	fmt.Println(allMsg)
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
	selfTeamUsers, enemyTeamUsers := score.getAllUsersFromSession(selfID, session)
	_ = selfTeamUsers
	summonerIDList := enemyTeamUsers

	fmt.Println("敌方队伍人员列表:", summonerIDList)
	if len(summonerIDList) == 0 {
		return
	}
	// 查询所有用户的信息并计算得分
	g := errgroup.Group{}
	summonerScores := make([]*score.UserScore, 0, 5)
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
	slices.SortFunc(summonerScores, func(a, b *score.UserScore) int {
		return cmp.Compare(b.Score, a.Score)
	})
	// 根据所有用户的分数判断小代上等马中等马下等马
	allMsg := ""
	for _, score := range summonerScores {
		horse := score.Judge(score.Score)
		currKDASb := strings.Builder{}
		for i := 0; i < 5 && i < len(score.CurrKDA); i++ {
			currKDASb.WriteString(fmt.Sprintf("%d/%d/%d  ", score.CurrKDA[i][0], score.CurrKDA[i][1],
				score.CurrKDA[i][2]))
		}
		currKDAMsg := currKDASb.String()
		msg := fmt.Sprintf("%s(%d): %s %s", horse, int(score.Score), score.SummonerName, currKDAMsg)
		allMsg += msg + "\n"
	}
	fmt.Println(allMsg)
}

// 根据客户端推送的信息，实时更新客户端状态
func (ts *TalentScout) onGameFlowUpdate(gameFlow string) {
	fmt.Println("切换状态:" + gameFlow)
	switch gameFlow {
	case string(models.GameFlowChampionSelect): //英雄选择
		fmt.Println("进入英雄选择阶段,正在计算用户分数")
		ts.updateGameState(GameStateChampSelect)
		//开个协程去计算队友分数
		go ts.CalcTeamScore()
	case string(models.GameFlowNone): //无状态
		ts.updateGameState(GameStateNone)
	case string(models.GameFlowMatchmaking): //匹配中
		ts.updateGameState(GameStateMatchmaking)
	case string(models.GameFlowInProgress): //游戏中
		fmt.Println("已进入游戏,正在计算敌方分数")
		ts.updateGameState(GameStateInGame)
		//开个协程去计算地方分数
		go ts.CalcEnemyTeamScore()
	case string(models.GameFlowReadyCheck): //找到对局，正在等待确认或拒绝
		ts.updateGameState(GameStateReadyCheck)
	default:
		ts.updateGameState(GameStateOther)
	}

}

func (ts *TalentScout) InitGameFlowMonitor(port int, authPwd string) error {
	//定义了一个自定义的网络拨号器（dialer），用于通过TCP协议连接到指定的服务器地址。
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	//它试图通过一系列本地IP地址来尝试与目标服务器建立连接，如果第一次尝试失败，则会使用下一个本地IP地址进行重试，最多尝试10次
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
	authSecret := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("riot:%s", authPwd)))
	header.Set("Authorization", "Basic "+authSecret)
	u, _ := url.Parse(rawUrl)
	c, _, err := dialer.Dial(u.String(), header)
	if err != nil {
		return err
	}
	fmt.Println(fmt.Sprintf("[成功连接到LOL客户端] %s", u.String()))
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
			//TODO
		default:

		}
	}
}

func main() {
	//先生成一个控制中心
	talentScout := NewTalentScout()
	//获取LCU客户端token和port
	pids, _ := getProcessPidByName(lolUxProcessName)
	cmdLine, err := GetProcessCommandLine(uint32(pids[0]))
	if err != nil {
		fmt.Printf("无法获取进程命令行: %v\n", err)
	}
	btsChunk := lolCommandlineReg.FindSubmatch([]byte(cmdLine))
	if len(btsChunk) < 3 {
		fmt.Println("格式错误")
	}
	token := string(btsChunk[1])
	port, _ := strconv.Atoi(string(btsChunk[2]))

	//先持久化一个客户端连接
	lcu.InitCli(port, token)
	//基于wss与客户端建立一个实时通讯
	err = talentScout.InitGameFlowMonitor(port, token)
	if err != nil {
		fmt.Println("连接失败")
	}
}
