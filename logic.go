package LOLTalentScout

import (
	"fmt"
	"github.com/avast/retry-go"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"main.go/lcu"
	"main.go/lcu/models"
	"main.go/scores"
	"sync"
	"time"
)

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

type lcuWsEvt string

const (
	onJsonApiEventPrefixLen              = len(`[8,"OnJsonApiEvent",`)
	gameFlowChangedEvt          lcuWsEvt = "/lol-gameflow/v1/gameflow-phase"
	champSelectUpdateSessionEvt lcuWsEvt = "/lol-champ-select/v1/session"
)

const (
	defaultScore       = 100 // 默认分数
	minGameDurationSec = 15 * 60
)

func acceptGame() error {
	err := lcu.AcceptGame()
	if err != nil {
		fmt.Println("自动接受对局失败")
		return err
	}
	return nil
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
func listSummoner(summonerIDList []int64) (map[int64]*models.Summoner, error) {
	list, err := lcu.ListSummoner(summonerIDList)
	if err != nil {
		return nil, err
	}
	res := make(map[int64]*models.Summoner, len(summonerIDList))
	for _, summoner := range list {
		summoner := summoner
		res[summoner.SummonerId] = &summoner
	}
	return res, nil
}

// getSummonerIDListFromConversationMsgList 从聊天室拿到五人id
func getSummonerIDListFromConversationMsgList(msgList []models.ConversationMsg) []int64 {
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
func listGameHistory(puuid string) ([]models.GameInfo, error) {
	limit := 20
	fmtList := make([]models.GameInfo, 0, limit)
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
func GetUserScore(summoner *models.Summoner) (*scores.UserScore, error) {

	summonerID := summoner.SummonerId
	userScoreInfo := &scores.UserScore{
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
	gameSummaryList := make([]models.GameSummary, 0, len(gameList))
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
			var gameSummary *models.GameSummary
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
		gameScore, err := scores.CalcUserGameScore(summonerID, gameSummary)
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

// GetAllUsersFromSession 对局开始后通过session拿到队友信息
func GetAllUsersFromSession(selfID int64, session *models.GameFlowSession) (selfTeamUsers []int64, enemyTeamUsers []int64) {
	selfTeamUsers = make([]int64, 0, 5)
	enemyTeamUsers = make([]int64, 0, 5)
	selfTeamID := models.TeamIDNone
	for _, teamUser := range session.GameData.TeamOne {
		summonerID := int64(teamUser.SummonerId)
		if selfID == summonerID {
			selfTeamID = models.TeamIDBlue
			break
		}
	}
	if selfTeamID == models.TeamIDNone {
		for _, teamUser := range session.GameData.TeamTwo {
			summonerID := int64(teamUser.SummonerId)
			if selfID == summonerID {
				selfTeamID = models.TeamIDRed
				break
			}
		}
	}
	if selfTeamID == models.TeamIDNone {
		return
	}
	for _, user := range session.GameData.TeamOne {
		userID := int64(user.SummonerId)
		if userID <= 0 {
			return
		}
		if models.TeamIDBlue == selfTeamID {
			selfTeamUsers = append(selfTeamUsers, userID)
		} else {
			enemyTeamUsers = append(enemyTeamUsers, userID)
		}
	}
	for _, user := range session.GameData.TeamTwo {
		userID := int64(user.SummonerId)
		if userID <= 0 {
			return
		}
		if models.TeamIDRed == selfTeamID {
			selfTeamUsers = append(selfTeamUsers, userID)
		} else {
			enemyTeamUsers = append(enemyTeamUsers, userID)
		}
	}
	return
}

// SendMessage 每隔两秒发送马匹消息
func SendMessage(msgList []string, sessionId string) {
	m := "LOL伯乐正在寻找千里马..."
	_ = lcu.SendConversationMsg(m, sessionId)
	for _, msg := range msgList {
		time.Sleep(4 * time.Second)
		_ = lcu.SendConversationMsg(msg, sessionId)
	}
	return
}
