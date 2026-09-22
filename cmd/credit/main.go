package main

// credit — TRAE SOLO 账号积分与签到状态报表。
// 补齐 credit.sh 引用但源码树缺失的 ./cmd/credit：
// 只读工具，逐账号查询签到状态与积分用量，不发起签到/领取。
//
// 用法:
//   credit             全部账号人类可读日报
//   credit -json       全部账号 JSON 数组
//   credit <uid>       指定账号（配合 -json 输出 JSON）

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"trae2api/internal/auth"
	"trae2api/internal/upstream"
)

type row struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	CheckedIn bool   `json:"checked_in"`
	Enable    bool   `json:"enable"`
	Credits   int64  `json:"credits"`
	Err       string `json:"err,omitempty"`
}

func main() {
	dir := flag.String("dir", "auths", "auths 目录")
	jsonOut := flag.Bool("json", false, "输出 JSON 数组")
	packs := flag.Bool("packs", false, "展示权益包明细（含积分过期时间）")
	flag.Parse()
	uidFilter := ""
	if flag.NArg() > 0 {
		uidFilter = flag.Arg(0)
	}

	auths, err := auth.LoadDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load auths:", err)
		os.Exit(1)
	}
	up := upstream.New()

	// -packs：权益包明细（含积分过期时间）
	if *packs {
		for _, a := range auths {
			if uidFilter != "" && a.UID != uidFilter {
				continue
			}
			list, err := up.PackList(a)
			if err != nil {
				fmt.Printf("== %s (%s) 查询失败: %v\n", a.Nickname, a.UID, err)
				continue
			}
			fmt.Printf("== %s (%s) 权益包 %d 个（按过期时间升序）==\n", a.Nickname, a.UID, len(list))
			for _, p := range list {
				remain := p.Limit - p.Used
				warn := ""
				if remain > 0 && p.ExpireAt > 0 && time.Until(time.Unix(p.ExpireAt, 0)) < 7*24*time.Hour {
					warn = "  ⚠️ 未用完即将过期"
				}
				fmt.Printf("  %s · 已用 %d/%d · 过期 %s%s\n",
					p.Desc, p.Used, p.Limit,
					time.Unix(p.ExpireAt, 0).Format("2006-01-02 15:04"), warn)
			}
		}
		return
	}

	var rows []row
	for _, a := range auths {
		if uidFilter != "" && a.UID != uidFilter {
			continue
		}
		r := row{UID: a.UID, Nickname: a.Nickname}
		checkedIn, credits, enable, err := up.CheckinStatus(a)
		if err != nil {
			r.Err = err.Error()
		} else {
			r.CheckedIn, r.Enable, r.Credits = checkedIn, enable, credits
			// 积分以用量接口为准（与调度器口径一致）
			if remain, err := up.UserEntUsage(a); err == nil {
				r.Credits = remain
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })

	if *jsonOut {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return
	}
	fmt.Printf("%-20s %-10s %-8s %-10s %s\n", "UID", "昵称", "签到", "积分", "备注")
	for _, r := range rows {
		st := "未签"
		if r.CheckedIn {
			st = "已签"
		}
		note := r.Err
		if !r.Enable && note == "" {
			note = "上游不可签"
		}
		fmt.Printf("%-20s %-10s %-8s %-10d %s\n", r.UID, r.Nickname, st, r.Credits, note)
	}
}
