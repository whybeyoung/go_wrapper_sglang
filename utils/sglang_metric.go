package utils

import (
	"strings"
)

type ContentItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Metric struct {
	Name    string        `json:"name"`
	Type    string        `json:"type"`
	Content []ContentItem `json:"content"`
}

func ParseMetrics(text string) map[string]*Metric {
	lines := strings.Split(text, "\n")
	metrics := make(map[string]*Metric, len(lines))
	var currentMetric *Metric

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "# HELP"):
			continue

		case strings.HasPrefix(line, "# TYPE"):
			parts := strings.Fields(line)
			if len(parts) < 4 {
				continue
			}

			currentMetric = &Metric{
				Name:    parts[2],
				Type:    parts[3],
				Content: []ContentItem{},
			}
			metrics[parts[2]] = currentMetric

		default:
			if currentMetric == nil {
				continue
			}
			keyValue := strings.SplitN(line, " ", 2)
			if len(keyValue) == 2 {
				currentMetric.Content = append(currentMetric.Content, ContentItem{
					Key:   keyValue[0],
					Value: keyValue[1],
				})
			}
		}
	}

	return metrics
}
