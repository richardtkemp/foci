package tools

import (
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
)

// HTMLToMarkdown converts extracted HTML to Markdown for web_fetch and for
// HTML attachments (agent.convertHTML), so both render the same way.
//
// It is htmltomarkdown.ConvertString plus the table plugin (#3241). Without
// the plugin a table's cells are inline text with no separator, so a table
// came out as one run ("Government websitesusa.gov, canada.caGaming…") and
// the reader could not tell which value belonged to which row. With it, a
// table is a Markdown table, one row per line.
func HTMLToMarkdown(htmlContent string) (string, error) {
	conv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
			table.NewTablePlugin(),
		),
	)
	return conv.ConvertString(htmlContent)
}
