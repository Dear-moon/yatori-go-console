package zhihuishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"yatori-go-console/config"

	"github.com/yatori-dev/yatori-go-core/api/zhihuishu"
	"github.com/yatori-dev/yatori-go-core/que-core/aiq"
	"github.com/yatori-dev/yatori-go-core/que-core/external"
	"github.com/yatori-dev/yatori-go-core/que-core/qentity"
	"golang.org/x/net/html"
)

func StudyMastery(ctx context.Context, setting config.Setting, user config.User, client *zhihuishu.Client, course zhihuishu.AICourse, output io.Writer) error {
	settings := user.CoursesCustom
	if settings.AutoExam != 1 && settings.AutoExam != 2 {
		return fmt.Errorf("智慧树 autoExam 仅支持 0（关闭）、1（AI）、2（外部题库）")
	}
	if settings.ExamAutoSubmit < 0 || settings.ExamAutoSubmit > 2 {
		return fmt.Errorf("智慧树 examAutoSubmit 仅支持 0、1、2")
	}
	if settings.AutoExam == 1 && setting.AiSetting.APIKEY == "" {
		return fmt.Errorf("智慧树掌握度答题需要配置 AI 密钥")
	}
	if settings.AutoExam == 2 && setting.ApiQueSetting.Url == "" {
		return fmt.Errorf("智慧树掌握度答题需要配置外部题库地址")
	}
	stats, err := client.Mastery(ctx, course)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "[智慧树] 开始掌握度练习，当前平均掌握度 %.0f%%。\n", stats.Percentage)
	completed := 0
	for _, theme := range stats.Themes {
		for _, point := range theme.Points {
			if point.FreeExam == 1 || point.Percentage >= 100 {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintf(output, "[智慧树] 掌握度知识点：%s（%.0f%%）\n", safeText(point.Name), point.Percentage)
			paper, err := client.StartMasteryPaper(ctx, course, point)
			if err != nil {
				return fmt.Errorf("知识点“%s”建卷失败：%w", safeText(point.Name), err)
			}
			if paper.QuestionCount == 0 {
				fmt.Fprintln(output, "[智慧树] 此知识点暂无练习题。")
				continue
			}
			questions, err := client.MasteryQuestions(ctx, course, paper)
			if err != nil {
				return err
			}
			paper.QuestionCount = len(questions)
			if paper.QuestionCount == 0 {
				return fmt.Errorf("知识点“%s”的实际试卷没有题目", safeText(point.Name))
			}
			answers := make([]string, len(questions))
			for i, question := range questions {
				if err := ctx.Err(); err != nil {
					return err
				}
				answers[i], err = masteryAnswer(setting, settings.AutoExam, question)
				if err != nil {
					return fmt.Errorf("知识点“%s”第 %d 题获取答案失败，未提交该练习：%w", safeText(point.Name), i+1, err)
				}
				fmt.Fprintf(output, "[智慧树] 已获取答案 %d/%d（%s）。\n", i+1, len(questions), safeText(question.TypeName))
			}
			for i, question := range questions {
				if err := client.SaveMasteryAnswer(ctx, course, paper, question, answers[i]); err != nil {
					return err
				}
			}
			if settings.ExamAutoSubmit == 0 {
				fmt.Fprintln(output, "[智慧树] 答案已保存，按 examAutoSubmit=0 保留待手动提交。")
				continue
			}
			result, err := client.SubmitMasteryPaper(ctx, course, paper)
			if err != nil {
				return err
			}
			updated, err := client.Mastery(ctx, course)
			if err != nil {
				return err
			}
			completed++
			fmt.Fprintf(output, "[智慧树] 练习已提交（状态 %d），课程平均掌握度 %.0f%%。\n", result.State, updated.Percentage)
		}
	}
	fmt.Fprintf(output, "[智慧树] 本轮掌握度练习完成，已提交 %d 个知识点。\n", completed)
	return nil
}

func masteryAnswer(setting config.Setting, mode int, question zhihuishu.MasteryQuestion) (string, error) {
	if question.Type != 1 && question.Type != 2 && question.Type != 14 {
		return "", fmt.Errorf("暂不支持的题型：%s", question.TypeName)
	}
	content, err := masteryText(question.Content)
	if err != nil {
		return "", err
	}
	topic := qentity.Question{Type: question.TypeName, Content: content}
	for _, option := range question.Options {
		text, err := masteryText(option.Content)
		if err != nil {
			return "", err
		}
		topic.Options = append(topic.Options, text)
	}
	var answers []string
	switch mode {
	case 1:
		messages := aiq.BuildAiQuestionMessage(topic)
		messages.Messages = append(messages.Messages, aiq.Message{Role: "system", Content: "仅使用题目提供的选项内容作答。不能确定答案时返回空 JSON 数组 []，不要随机猜测。"})
		ai := setting.AiSetting
		response, err := aiq.AggregationAIApi(ai.AiUrl, ai.Model, ai.AiType, messages, ai.APIKEY)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal([]byte(response), &answers); err != nil {
			return "", fmt.Errorf("AI 返回的答案不是字符串数组")
		}
	case 2:
		result, err := external.ApiQueRequest(topic, setting.ApiQueSetting.Url, 1, nil)
		if err != nil {
			return "", err
		}
		if result.Code != 200 {
			return "", fmt.Errorf("题库未找到答案（状态 %d）", result.Code)
		}
		answers = result.Answers
	}
	return encodeMasteryAnswer(question, answers)
}

func masteryText(value string) (string, error) {
	tokenizer := html.NewTokenizer(strings.NewReader(value))
	var text strings.Builder
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() != io.EOF {
				return "", tokenizer.Err()
			}
			return strings.Join(strings.Fields(text.String()), " "), nil
		case html.TextToken:
			text.Write(tokenizer.Text())
		case html.StartTagToken, html.SelfClosingTagToken:
			tag, _ := tokenizer.TagName()
			switch string(tag) {
			case "img":
				return "", fmt.Errorf("题目含图片，需要人工核对")
			case "p", "div", "br", "li":
				text.WriteByte(' ')
			}
		case html.EndTagToken:
			tag, _ := tokenizer.TagName()
			switch string(tag) {
			case "p", "div", "li":
				text.WriteByte(' ')
			}
		}
	}
}

func normalizeMasteryAnswer(answer string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, answer)
}

func masteryTruth(answer string) string {
	switch answer {
	case "正确", "对", "是", "true", "True":
		return "true"
	case "错误", "错", "否", "false", "False":
		return "false"
	}
	return answer
}

func encodeMasteryAnswer(question zhihuishu.MasteryQuestion, answers []string) (string, error) {
	if len(answers) == 0 {
		return "", fmt.Errorf("未获得有效答案")
	}
	if question.Type != 2 && len(answers) != 1 {
		return "", fmt.Errorf("单选或判断题必须只有一个答案")
	}
	selected := make(map[int]bool, len(answers))
	for _, answer := range answers {
		answer = normalizeMasteryAnswer(answer)
		if answer == "" {
			return "", fmt.Errorf("答案为空")
		}
		match := -1
		for i, option := range question.Options {
			text, err := masteryText(option.Content)
			if err != nil {
				return "", err
			}
			text = normalizeMasteryAnswer(text)
			equal := text == answer || string(option.ID) == answer
			if question.Type == 14 {
				equal = equal || masteryTruth(text) == masteryTruth(answer)
			}
			if equal {
				if match >= 0 {
					return "", fmt.Errorf("答案匹配到多个选项")
				}
				match = i
			}
		}
		if match < 0 && len(answer) == 1 && answer[0] >= 'A' && int(answer[0]-'A') < len(question.Options) {
			match = int(answer[0] - 'A')
		}
		if match < 0 {
			return "", fmt.Errorf("答案无法匹配题目选项")
		}
		if selected[match] {
			return "", fmt.Errorf("答案重复")
		}
		selected[match] = true
	}
	var ids []string
	for i, option := range question.Options {
		if selected[i] {
			ids = append(ids, string(option.ID))
		}
	}
	return strings.Join(ids, "#@#"), nil
}
