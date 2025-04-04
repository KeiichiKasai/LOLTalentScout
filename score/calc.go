package score

import (
	"errors"
	"main.go/lcu"
	"main.go/lcu/models"
	"main.go/utils"
	"sync"
)

const (
	defaultScore = 100
)

type (
	RateItemConf struct {
		Limit     float64      `json:"limit" required:"true"`     // >30%
		ScoreConf [][2]float64 `json:"scoreConf" required:"true"` // [ [最低人头限制,加分数] ]
	}
	HorseScoreConf struct {
		Score float64 `json:"score,omitempty" required:"true"`
		Name  string  `json:"name" required:"true"`
	}
	CalcScoreConf struct {
		Enabled            bool              `json:"enabled" default:"false"`
		FirstBlood         [2]float64        `json:"firstBlood" required:"true"`         // [击杀+,助攻+]
		PentaKills         [1]float64        `json:"pentaKills" required:"true"`         // 五杀
		QuadraKills        [1]float64        `json:"quadraKills" required:"true"`        // 四杀
		TripleKills        [1]float64        `json:"tripleKills" required:"true"`        // 三杀
		JoinTeamRateRank   [4]float64        `json:"joinTeamRate" required:"true"`       // 参团率排名
		GoldEarnedRank     [4]float64        `json:"goldEarned" required:"true"`         // 打钱排名
		HurtRank           [2]float64        `json:"hurtRank" required:"true"`           // 伤害排名
		Money2hurtRateRank [2]float64        `json:"money2HurtRateRank" required:"true"` // 金钱转换伤害比排名
		VisionScoreRank    [2]float64        `json:"visionScoreRank" required:"true"`    // 视野得分排名
		MinionsKilled      [][2]float64      `json:"minionsKilled" required:"true"`      // 补兵 [ [补兵数,加分数] ]
		KillRate           []RateItemConf    `json:"killRate" required:"true"`           // 人头占比
		HurtRate           []RateItemConf    `json:"hurtRate" required:"true"`           // 伤害占比
		AssistRate         []RateItemConf    `json:"assistRate" required:"true"`         // 助攻占比
		AdjustKDA          [2]float64        `json:"adjustKDA" required:"true"`          // kda
		Horse              [6]HorseScoreConf `json:"horse" required:"true"`
	}
)

// CalcScore 各项得分标准
var CalcScore = CalcScoreConf{
	Enabled:            true,
	FirstBlood:         [2]float64{10, 5},
	PentaKills:         [1]float64{20},
	QuadraKills:        [1]float64{10},
	TripleKills:        [1]float64{5},
	JoinTeamRateRank:   [4]float64{10, 5, 5, 10},
	GoldEarnedRank:     [4]float64{10, 5, 5, 10},
	HurtRank:           [2]float64{10, 5},
	Money2hurtRateRank: [2]float64{10, 5},
	VisionScoreRank:    [2]float64{10, 5},
	MinionsKilled: [][2]float64{
		{10, 20},
		{9, 10},
		{8, 5},
	},
	KillRate: []RateItemConf{
		{Limit: 50, ScoreConf: [][2]float64{
			{15, 40},
			{10, 20},
			{5, 10},
		}},
		{Limit: 40, ScoreConf: [][2]float64{
			{15, 20},
			{10, 10},
			{5, 5},
		}},
	},
	HurtRate: []RateItemConf{
		{Limit: 40, ScoreConf: [][2]float64{
			{15, 40},
			{10, 20},
			{5, 10},
		}},
		{Limit: 30, ScoreConf: [][2]float64{
			{15, 20},
			{10, 10},
			{5, 5},
		}},
	},
	AssistRate: []RateItemConf{
		{Limit: 50, ScoreConf: [][2]float64{
			{20, 30},
			{18, 25},
			{15, 20},
			{10, 10},
			{5, 5},
		}},
		{Limit: 40, ScoreConf: [][2]float64{
			{20, 15},
			{15, 10},
			{10, 5},
			{5, 3},
		}},
	},
	AdjustKDA: [2]float64{2, 5},
	Horse: [6]HorseScoreConf{
		{Score: 180, Name: "通天代"},
		{Score: 150, Name: "小代"},
		{Score: 125, Name: "上等马"},
		{Score: 105, Name: "中等马"},
		{Score: 95, Name: "下等马"},
		{Score: 0.0001, Name: "牛马"},
	},
}

var confMu = sync.Mutex{}

func CalcUserGameScore(summonerID int64, gameSummary lcu.GameSummary) (*ScoreWithReason, error) {
	//算分需要的信息ScoreConf
	confMu.Lock()
	calcScoreConf := CalcScore
	confMu.Unlock()

	gameScore := NewScoreWithReason(defaultScore)
	var userParticipantId int
	for _, identity := range gameSummary.ParticipantIdentities {
		if identity.Player.SummonerId == summonerID {
			userParticipantId = identity.ParticipantId
		}
	}
	if userParticipantId == 0 {
		return nil, errors.New("获取用户位置失败")
	}
	var userTeamID *models.TeamID
	memberParticipantIDList := make([]int, 0, 4)
	idMapParticipant := make(map[int]lcu.Participant, len(gameSummary.Participants))
	for _, item := range gameSummary.Participants {
		if item.ParticipantId == userParticipantId {
			userTeamID = &item.TeamId
		}
		idMapParticipant[item.ParticipantId] = item
	}
	if userTeamID == nil {
		return nil, errors.New("获取用户队伍id失败")
	}
	for _, item := range gameSummary.Participants {
		//如果参与者和该用户一个阵营，就加入到memberParticipantIDList
		if item.TeamId == *userTeamID {
			memberParticipantIDList = append(memberParticipantIDList, item.ParticipantId)
		}
	}
	//全队信息
	totalKill := 0   // 总人头
	totalDeath := 0  // 总死亡
	totalAssist := 0 // 总助攻
	totalHurt := 0   // 总伤害
	totalMoney := 0  // 总金钱
	for _, participant := range gameSummary.Participants {
		if participant.TeamId != *userTeamID {
			continue
		}
		totalKill += participant.Stats.Kills
		totalDeath += participant.Stats.Deaths
		totalAssist += participant.Stats.Assists
		totalHurt += participant.Stats.TotalDamageDealtToChampions
		totalMoney += participant.Stats.GoldEarned
	}

	userParticipant := idMapParticipant[userParticipantId]
	isSupportRole := userParticipant.Timeline.Lane == models.LaneBottom &&
		userParticipant.Timeline.Role == models.ChampionRoleSupport
	// 一血击杀
	if userParticipant.Stats.FirstBloodKill {
		gameScore.Add(calcScoreConf.FirstBlood[0], ScoreOptionFirstBloodKill)
		// 一血助攻
	} else if userParticipant.Stats.FirstBloodAssist {
		gameScore.Add(calcScoreConf.FirstBlood[1], ScoreOptionFirstBloodAssist)
	}
	// 五杀
	if userParticipant.Stats.PentaKills > 0 {
		gameScore.Add(calcScoreConf.PentaKills[0], ScoreOptionPentaKills)
		// 四杀
	} else if userParticipant.Stats.QuadraKills > 0 {
		gameScore.Add(calcScoreConf.QuadraKills[0], ScoreOptionQuadraKills)
		// 三杀
	} else if userParticipant.Stats.TripleKills > 0 {
		gameScore.Add(calcScoreConf.TripleKills[0], ScoreOptionTripleKills)
	}
	// 参团率
	if totalKill > 0 {
		joinTeamRateRank := 1
		userJoinTeamKillRate := float64(userParticipant.Stats.Assists+userParticipant.Stats.Kills) / float64(
			totalKill)
		memberJoinTeamKillRates := listMemberJoinTeamKillRates(&gameSummary, totalKill, memberParticipantIDList)
		for _, rate := range memberJoinTeamKillRates {
			if rate > userJoinTeamKillRate {
				joinTeamRateRank++
			}
		}
		if joinTeamRateRank == 1 {
			gameScore.Add(calcScoreConf.JoinTeamRateRank[0], ScoreOptionJoinTeamRateRank)
		} else if joinTeamRateRank == 2 {
			gameScore.Add(calcScoreConf.JoinTeamRateRank[1], ScoreOptionJoinTeamRateRank)
		} else if joinTeamRateRank == 4 {
			gameScore.Add(-calcScoreConf.JoinTeamRateRank[2], ScoreOptionJoinTeamRateRank)
		} else if joinTeamRateRank == 5 {
			gameScore.Add(-calcScoreConf.JoinTeamRateRank[3], ScoreOptionJoinTeamRateRank)
		}
	}
	// 获取金钱
	if totalMoney > 0 {
		moneyRank := 1
		userMoney := userParticipant.Stats.GoldEarned
		memberMoneyList := listMemberMoney(&gameSummary, memberParticipantIDList)
		for _, v := range memberMoneyList {
			if v > userMoney {
				moneyRank++
			}
		}
		if moneyRank == 1 {
			gameScore.Add(calcScoreConf.GoldEarnedRank[0], ScoreOptionGoldEarnedRank)
		} else if moneyRank == 2 {
			gameScore.Add(calcScoreConf.GoldEarnedRank[1], ScoreOptionGoldEarnedRank)
		} else if moneyRank == 4 && !isSupportRole {
			gameScore.Add(-calcScoreConf.GoldEarnedRank[2], ScoreOptionGoldEarnedRank)
		} else if moneyRank == 5 && !isSupportRole {
			gameScore.Add(-calcScoreConf.GoldEarnedRank[3], ScoreOptionGoldEarnedRank)
		}
	}
	// 伤害占比
	if totalHurt > 0 {
		hurtRank := 1
		userHurt := userParticipant.Stats.TotalDamageDealtToChampions
		memberHurtList := listMemberHurt(&gameSummary, memberParticipantIDList)
		for _, v := range memberHurtList {
			if v > userHurt {
				hurtRank++
			}
		}
		if hurtRank == 1 {
			gameScore.Add(calcScoreConf.HurtRank[0], ScoreOptionHurtRank)
		} else if hurtRank == 2 {
			gameScore.Add(calcScoreConf.HurtRank[1], ScoreOptionHurtRank)
		}
	}
	// 金钱转换伤害比
	if totalMoney > 0 && totalHurt > 0 {
		money2hurtRateRank := 1
		userMoney2hurtRate := float64(userParticipant.Stats.TotalDamageDealtToChampions) / float64(userParticipant.Stats.
			GoldEarned)
		memberMoney2hurtRateList := listMemberMoney2hurtRate(&gameSummary, memberParticipantIDList)
		for _, v := range memberMoney2hurtRateList {
			if v > userMoney2hurtRate {
				money2hurtRateRank++
			}
		}
		if money2hurtRateRank == 1 {
			gameScore.Add(calcScoreConf.Money2hurtRateRank[0], ScoreOptionMoney2hurtRateRank)
		} else if money2hurtRateRank == 2 {
			gameScore.Add(calcScoreConf.Money2hurtRateRank[1], ScoreOptionMoney2hurtRateRank)
		}
	}
	// 视野得分
	{
		visionScoreRank := 1
		userVisionScore := userParticipant.Stats.VisionScore
		memberVisionScoreList := listMemberVisionScore(&gameSummary, memberParticipantIDList)
		for _, v := range memberVisionScoreList {
			if v > userVisionScore {
				visionScoreRank++
			}
		}
		if visionScoreRank == 1 {
			gameScore.Add(calcScoreConf.VisionScoreRank[0], ScoreOptionVisionScoreRank)
		} else if visionScoreRank == 2 {
			gameScore.Add(calcScoreConf.VisionScoreRank[1], ScoreOptionVisionScoreRank)
		}
	}
	// 补兵 每分钟8个刀以上加5分 ,9+10, 10+20
	{
		totalMinionsKilled := userParticipant.Stats.TotalMinionsKilled
		gameDurationMinute := gameSummary.GameDuration / 60
		minuteMinionsKilled := totalMinionsKilled / gameDurationMinute
		for _, minionsKilledLimit := range calcScoreConf.MinionsKilled {
			if minuteMinionsKilled >= int(minionsKilledLimit[0]) {
				gameScore.Add(minionsKilledLimit[1], ScoreOptionMinionsKilled)
				break
			}
		}
	}
	// 人头占比
	if totalKill > 0 {
		// 人头占比>50%
		userKillRate := float64(userParticipant.Stats.Kills) / float64(totalKill)
	userKillRateLoop:
		for _, killRateConfItem := range calcScoreConf.KillRate {
			if userKillRate > killRateConfItem.Limit {
				for _, limitConf := range killRateConfItem.ScoreConf {
					if userParticipant.Stats.Kills > int(limitConf[0]) {
						gameScore.Add(limitConf[1], ScoreOptionKillRate)
						break userKillRateLoop
					}
				}
			}
		}
	}
	// 伤害占比
	if totalHurt > 0 {
		// 伤害占比>50%
		userHurtRate := float64(userParticipant.Stats.TotalDamageDealtToChampions) / float64(totalHurt)
	userHurtRateLoop:
		for _, killRateConfItem := range calcScoreConf.HurtRate {
			if userHurtRate > killRateConfItem.Limit {
				for _, limitConf := range killRateConfItem.ScoreConf {
					if userParticipant.Stats.Kills > int(limitConf[0]) {
						gameScore.Add(limitConf[1], ScoreOptionHurtRate)
						break userHurtRateLoop
					}
				}
			}
		}
	}
	// 助攻占比
	if totalAssist > 0 {
		// 助攻占比>50%
		userAssistRate := float64(userParticipant.Stats.Assists) / float64(totalAssist)
	userAssistRateLoop:
		for _, killRateConfItem := range calcScoreConf.AssistRate {
			if userAssistRate > killRateConfItem.Limit {
				for _, limitConf := range killRateConfItem.ScoreConf {
					if userParticipant.Stats.Kills > int(limitConf[0]) {
						gameScore.Add(limitConf[1], ScoreOptionAssistRate)
						break userAssistRateLoop
					}
				}
			}
		}
	}
	userJoinTeamKillRate := 1.0
	if totalKill > 0 {
		userJoinTeamKillRate = float64(userParticipant.Stats.Assists+userParticipant.Stats.Kills) / float64(
			totalKill)
	}
	userDeathTimes := userParticipant.Stats.Deaths
	if userParticipant.Stats.Deaths == 0 {
		userDeathTimes = 1
	}
	adjustVal := (float64(userParticipant.Stats.Kills+userParticipant.Stats.Assists)/float64(userDeathTimes) -
		calcScoreConf.AdjustKDA[0] +
		float64(userParticipant.Stats.Kills-userParticipant.Stats.Deaths)/calcScoreConf.AdjustKDA[1]) * userJoinTeamKillRate
	// log.Printf("game: %d,kda: %d/%d/%d\n", gameSummary.GameId, userParticipant.Stats.Kills,
	// 	userParticipant.Stats.Deaths, userParticipant.Stats.Assists)
	gameScore.Add(adjustVal, ScoreOptionKDAAdjust)
	// kdaInfoStr := fmt.Sprintf("%d/%d/%d", userParticipant.Stats.Kills, userParticipant.Stats.Deaths,
	// 	userParticipant.Stats.Assists)
	// if global.IsDevMode() {
	// 	log.Printf("对局%d得分:%.2f, kda:%s,原因:%s", gameSummary.GameId, gameScore.Value(), kdaInfoStr, gameScore.Reasons2String())
	// }
	return gameScore, nil
}

func listMemberVisionScore(gameSummary *lcu.GameSummary, memberParticipantIDList []int) []int {
	res := make([]int, 0, 4)
	for _, participant := range gameSummary.Participants {
		if !utils.InArrayInt(participant.ParticipantId, memberParticipantIDList) {
			continue
		}
		res = append(res, participant.Stats.VisionScore)
	}
	return res
}

func listMemberMoney2hurtRate(gameSummary *lcu.GameSummary, memberParticipantIDList []int) []float64 {
	res := make([]float64, 0, 4)
	for _, participant := range gameSummary.Participants {
		if !utils.InArrayInt(participant.ParticipantId, memberParticipantIDList) {
			continue
		}
		res = append(res, float64(participant.Stats.TotalDamageDealtToChampions)/float64(participant.Stats.
			GoldEarned))
	}
	return res
}

func listMemberMoney(gameSummary *lcu.GameSummary, memberParticipantIDList []int) []int {
	res := make([]int, 0, 4)
	for _, participant := range gameSummary.Participants {
		if !utils.InArrayInt(participant.ParticipantId, memberParticipantIDList) {
			continue
		}
		res = append(res, participant.Stats.GoldEarned)
	}
	return res
}

func listMemberJoinTeamKillRates(gameSummary *lcu.GameSummary, totalKill int, memberParticipantIDList []int) []float64 {
	res := make([]float64, 0, 4)
	for _, participant := range gameSummary.Participants {
		if !utils.InArrayInt(participant.ParticipantId, memberParticipantIDList) {
			continue
		}
		res = append(res, float64(participant.Stats.Assists+participant.Stats.Kills)/float64(
			totalKill))
	}
	return res
}

func listMemberHurt(gameSummary *lcu.GameSummary, memberParticipantIDList []int) []int {
	res := make([]int, 0, 4)
	for _, participant := range gameSummary.Participants {
		if !utils.InArrayInt(participant.ParticipantId, memberParticipantIDList) {
			continue
		}
		res = append(res, participant.Stats.TotalDamageDealtToChampions)
	}
	return res
}
func getAllUsersFromSession(selfID int64, session *lcu.GameFlowSession) (selfTeamUsers []int64,
	enemyTeamUsers []int64) {
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

func Judge(score float64) string {
	switch {
	case score < 95:
		return "纯牛马"
	case score >= 95 && score < 105:
		return "下等马"
	case score >= 105 && score < 125:
		return "中等马"
	case score >= 125 && score < 150:
		return "上等马"
	case score >= 150 && score < 180:
		return "小代"
	case score >= 180:
		return "通天代"
	}
	return "唐"
}
