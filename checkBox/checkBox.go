package checkBox

import (
	"fmt"
	"github.com/getlantern/systray"
)

var (
	// 创建一个复选框菜单项
	acceptItem *systray.MenuItem
	isAccept   bool // 记录当前勾选状态
)

func OnStart() {
	systray.SetIcon(Icon)
	systray.SetTitle("LOLTalentScout")
	systray.SetTooltip("LOLTalentScout")
	// 创建复选框菜单项
	acceptItem = systray.AddMenuItemCheckbox("自动接受对局", "点击切换勾选状态", true)
	// 初始化状态
	acceptItem.Check()
	// 监听菜单项点击事件
	go func() {
		for {
			select {
			case <-acceptItem.ClickedCh:
				// 切换状态
				if acceptItem.Checked() {
					acceptItem.Uncheck()
					isAccept = false
				} else {
					acceptItem.Check()
					isAccept = true
				}
				//提醒用户
				if isAccept {
					fmt.Println("已设置自动接受对局")
				} else {
					fmt.Println("已取消自动接受对局")
				}
			}
		}
	}()
}

func OnExit() {
}
