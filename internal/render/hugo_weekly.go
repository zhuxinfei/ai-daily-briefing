package render

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"briefing-v3/internal/store"
)

// WriteWeeklyPost renders a weekly report as a Hextra blog post under
// {siteDir}/content/cn/blog/weekly/{isoYear}-W{ww}.md. The blog type
// gives it a card-list layout (no sidebar), matching the hubtoday pattern.
//
// dailyIssues should be sorted by date ascending; they are used to build
// the "本周日报" cross-link list at the top of the article.
func WriteWeeklyPost(
	siteDir string,
	weekly *store.WeeklyIssue,
	dailyIssues []*store.Issue,
) (string, error) {
	if siteDir == "" {
		return "", fmt.Errorf("weekly hugo: siteDir is empty")
	}
	if weekly == nil {
		return "", fmt.Errorf("weekly hugo: weekly is nil")
	}

	// Read basePath from hugo.yaml for GitHub Pages subpath support.
	basePath := ""
	if hugoYAML, err := os.ReadFile(filepath.Join(siteDir, "hugo.yaml")); err == nil {
		if m := regexp.MustCompile(`(?m)^baseURL:\s*https?://[^/]+(/.+?)/?$`).FindSubmatch(hugoYAML); len(m) > 1 {
			basePath = string(m[1])
		}
	}

	// --- directory ---
	weeklyDir := filepath.Join(siteDir, "content", "cn", "blog", "weekly")
	if err := os.MkdirAll(weeklyDir, 0o755); err != nil {
		return "", fmt.Errorf("weekly hugo: mkdir %s: %w", weeklyDir, err)
	}

	// --- filename ---
	filename := fmt.Sprintf("%d-W%02d.md", weekly.Year, weekly.Week)
	outPath := filepath.Join(weeklyDir, filename)

	// --- frontmatter ---
	title := weekly.Title
	if title == "" {
		title = fmt.Sprintf("第%d周 AI周报", weekly.Week)
	}
	description := fmt.Sprintf("%s ~ %s AI行业周度综合分析",
		weekly.StartDate.Format("2006-01-02"),
		weekly.EndDate.Format("2006-01-02"))

	var fm strings.Builder
	fm.WriteString("---\n")
	fmt.Fprintf(&fm, "title: %q\n", title)
	fmt.Fprintf(&fm, "date: %s\n", weekly.StartDate.Format("2006-01-02T15:04:05+08:00"))
	fm.WriteString("tags: [\"周报\", \"")
	fmt.Fprintf(&fm, "%d\"]\n", weekly.Year)
	fmt.Fprintf(&fm, "description: %q\n", description)
	fm.WriteString("---\n\n")

	// --- hero header card (大字报) ---
	weeklyDateStr := fmt.Sprintf("%d-W%02d", weekly.Year, weekly.Week)
	heroPrefix := ""
	heroSrc := findExistingPath([]string{
		filepath.Join("data", "images", "cards", weeklyDateStr, "header.png"),
		filepath.Join("/root/briefing-v3/data/images/cards", weeklyDateStr, "header.png"),
	})
	if heroSrc != "" {
		targetDir := filepath.Join(siteDir, "static", "images", "cards", weeklyDateStr)
		if err := os.MkdirAll(targetDir, 0o755); err == nil {
			targetPath := filepath.Join(targetDir, "header.png")
			if err := copyFile(heroSrc, targetPath); err == nil {
				heroPrefix = fmt.Sprintf("![AI 周报大字报 · %s](/images/cards/%s/header.png)\n\n",
					weeklyDateStr, weeklyDateStr)
			}
		}
	}

	// --- body ---
	var body strings.Builder
	body.WriteString(heroPrefix)

	// Header with date range and daily links.
	fmt.Fprintf(&body, "> %s ~ %s | 本周共 %d 期日报\n\n",
		weekly.StartDate.Format("2006-01-02"),
		weekly.EndDate.Format("2006-01-02"),
		len(dailyIssues))

	if len(dailyIssues) > 0 {
		body.WriteString("**本周日报**: ")
		for i, di := range dailyIssues {
			if di == nil {
				continue
			}
			d := di.IssueDate
			link := fmt.Sprintf("%s/%d/%s/%s/",
				basePath,
				d.Year(),
				d.Format("2006-01"),
				d.Format("2006-01-02"))
			if i > 0 {
				body.WriteString(" | ")
			}
			fmt.Fprintf(&body, "[%s](%s)", d.Format("01-02"), link)
		}
		body.WriteString("\n\n")
	}

	body.WriteString("---\n\n")

	// Sections.
	if s := strings.TrimSpace(weekly.FocusMD); s != "" {
		body.WriteString("## 本周聚焦\n\n")
		body.WriteString(s)
		body.WriteString("\n\n---\n\n")
	}
	if s := strings.TrimSpace(weekly.SignalsMD); s != "" {
		body.WriteString("## 信号与噪音\n\n")
		body.WriteString(s)
		body.WriteString("\n\n---\n\n")
	}
	if s := strings.TrimSpace(weekly.TrendsMD); s != "" {
		body.WriteString("## 宏观趋势\n\n")
		// Simple diagram first.
		if d := strings.TrimSpace(weekly.TrendsDiagram); d != "" {
			body.WriteString("```mermaid\n")
			body.WriteString(d)
			body.WriteString("\n```\n\n")
		}
		// Detailed diagram in collapsible.
		if d := strings.TrimSpace(weekly.TrendsDiagramDetail); d != "" {
			body.WriteString("<details>\n<summary>展开详细图谱</summary>\n\n")
			body.WriteString("```mermaid\n")
			body.WriteString(d)
			body.WriteString("\n```\n\n</details>\n\n")
		}
		body.WriteString(s)
		body.WriteString("\n\n---\n\n")
	}
	if s := strings.TrimSpace(weekly.TakeawaysMD); s != "" {
		body.WriteString("## 对我们的启发\n\n")
		body.WriteString(s)
		body.WriteString("\n\n---\n\n")
	}
	if s := strings.TrimSpace(weekly.PonderMD); s != "" {
		body.WriteString("## 本周思考\n\n")
		body.WriteString(s)
		body.WriteString("\n")
	}

	full := fm.String() + body.String()
	if err := os.WriteFile(outPath, []byte(full), 0o644); err != nil {
		return "", fmt.Errorf("weekly hugo: write %s: %w", outPath, err)
	}
	return outPath, nil
}
