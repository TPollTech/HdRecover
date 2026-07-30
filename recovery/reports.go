package recovery

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func writeAllReports(outDir string, result Result, runErr error) error {
	if err := writeReport(outDir, result, runErr); err != nil {
		return err
	}
	_ = writeJSONReport(outDir, result, runErr)
	_ = writeCSVReport(outDir, result)
	_ = writeHTMLReport(outDir, result, runErr)
	if len(result.BadRanges) > 0 {
		_ = writeBadMap(outDir, result.BadRanges)
	}
	return nil
}

func writeJSONReport(outDir string, result Result, runErr error) error {
	payload := struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
		Result Result `json:"result"`
	}{Status: "concluido", Result: result}
	if runErr != nil {
		payload.Status = "interrompido"
		payload.Error = runErr.Error()
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "RELATORIO_DA_RECUPERACAO.json"), data, 0o644)
}

func writeCSVReport(outDir string, result Result) error {
	f, err := os.Create(filepath.Join(outDir, "ARQUIVOS_RECUPERADOS.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"arquivo", "nome_original", "caminho_original", "extensao", "categoria", "tamanho", "integridade", "sha256", "duplicado_de", "metodo", "offset_origem", "observacoes"})
	for _, item := range result.RecoveredFiles {
		_ = w.Write([]string{
			item.Path, item.OriginalName, item.OriginalPath, item.Extension, item.Category,
			strconv.FormatInt(item.Size, 10), string(item.Integrity), item.HashSHA256,
			item.DuplicateOf, item.Method, strconv.FormatInt(item.SourceOffset, 10), item.Notes,
		})
	}
	w.Flush()
	closeErr := f.Close()
	if err := w.Error(); err != nil {
		return err
	}
	return closeErr
}

func writeHTMLReport(outDir string, result Result, runErr error) error {
	items := append([]RecoveredFile(nil), result.RecoveredFiles...)
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	categories := make(map[string]int)
	integrities := make(map[Integrity]int)
	duplicates := 0
	for _, item := range items {
		categories[item.Category]++
		integrities[item.Integrity]++
		if item.DuplicateOf != "" {
			duplicates++
		}
	}

	var b strings.Builder
	b.WriteString("<!doctype html><html lang=\"pt-BR\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>Resultados HdRecover</title><style>")
	b.WriteString("*{box-sizing:border-box}body{font-family:Segoe UI,Arial,sans-serif;background:#181a1e;color:#e8ebf0;margin:0;padding:28px}h1{margin:0 0 6px}.muted{color:#aeb5bf}.summary{display:flex;gap:14px;flex-wrap:wrap;margin:22px 0}.card{background:#22252b;border:1px solid #343941;border-radius:12px;padding:14px}.summary .card{min-width:140px}.toolbar{position:sticky;top:0;z-index:4;display:flex;gap:10px;flex-wrap:wrap;background:#181a1eea;backdrop-filter:blur(8px);padding:12px 0;margin-bottom:16px}.toolbar input,.toolbar select{background:#2b2f36;color:#e8ebf0;border:1px solid #444b55;border-radius:8px;padding:10px 12px;min-height:42px}.toolbar input[type=search]{min-width:min(420px,100%);flex:1}.toolbar label{display:flex;align-items:center;gap:7px;padding:0 8px}.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:14px}.thumb{width:100%;height:160px;object-fit:contain;background:#11151b;border-radius:8px}.media{width:100%;max-height:180px;background:#11151b;border-radius:8px}.good{color:#7ee787}.warn{color:#ffd166}.bad{color:#ff7b72}a{color:#69a7ff;text-decoration:none;word-break:break-all}code{font-size:11px}.item-title{font-weight:600;margin:8px 0 4px}.badge{display:inline-block;border:1px solid #47505b;border-radius:999px;padding:2px 8px;margin:5px 5px 0 0;font-size:12px}.hidden{display:none!important}.empty{display:none;text-align:center;padding:36px}.diskmap{position:relative;height:18px;background:#26303a;border-radius:9px;overflow:hidden;margin-top:10px}.badseg{position:absolute;top:0;bottom:0;background:#ff7b72;min-width:2px}.note{background:#262c34;border-left:4px solid #69a7ff;padding:12px 14px;border-radius:6px;margin:16px 0}.count{font-variant-numeric:tabular-nums}</style></head><body>")
	b.WriteString("<h1>HdRecover — resultados</h1><div class=\"muted\">Relatório gerado em " + html.EscapeString(time.Now().Format("02/01/2006 15:04:05")) + "</div>")
	status := "Concluído"
	if runErr != nil {
		status = "Interrompido: " + runErr.Error()
	}
	b.WriteString("<div class=\"summary\"><div class=\"card\"><b>Status</b><br>" + html.EscapeString(status) + "</div><div class=\"card\"><b>Arquivos únicos</b><br><span class=\"count\">" + strconv.Itoa(result.FilesFound) + "</span></div><div class=\"card\"><b>Duplicados evitados</b><br><span class=\"count\">" + strconv.Itoa(duplicates) + "</span></div><div class=\"card\"><b>Candidatos</b><br><span class=\"count\">" + strconv.Itoa(result.Candidates) + "</span></div><div class=\"card\"><b>Falhas de leitura</b><br><span class=\"count\">" + strconv.Itoa(result.ReadErrors) + "</span></div><div class=\"card\"><b>Tempo</b><br>" + html.EscapeString(formatDuration(result.Duration)) + "</div></div>")
	b.WriteString("<div class=\"note\">A classificação de integridade é estrutural e não garante que todo o conteúdo esteja perfeito. Abra os arquivos importantes para confirmar.</div>")
	if len(result.BadRanges) > 0 && result.BytesScanned > 0 {
		b.WriteString("<div class=\"card\"><b>Mapa aproximado de falhas de leitura</b><div class=\"muted\">Vermelho indica regiões que exigiram preenchimento ou não puderam ser lidas.</div><div class=\"diskmap\">")
		for _, bad := range result.BadRanges {
			left := float64(bad.Offset) / float64(result.BytesScanned) * 100
			width := float64(bad.Length) / float64(result.BytesScanned) * 100
			if left < 0 {
				left = 0
			}
			if left > 100 {
				continue
			}
			if width < 0.15 {
				width = 0.15
			}
			if left+width > 100 {
				width = 100 - left
			}
			b.WriteString(fmt.Sprintf("<span class=\"badseg\" title=\"%s — %s\" style=\"left:%.4f%%;width:%.4f%%\"></span>", html.EscapeString(humanBytes(bad.Offset)), html.EscapeString(humanBytes(bad.Length)), left, width))
		}
		b.WriteString("</div></div>")
	}
	b.WriteString("<div class=\"toolbar\"><input id=\"search\" type=\"search\" placeholder=\"Pesquisar nome, caminho original ou extensão…\"><select id=\"category\"><option value=\"\">Todas as categorias</option>")
	categoryNames := make([]string, 0, len(categories))
	for name := range categories {
		categoryNames = append(categoryNames, name)
	}
	sort.Strings(categoryNames)
	for _, name := range categoryNames {
		b.WriteString("<option value=\"" + html.EscapeString(strings.ToLower(name)) + "\">" + html.EscapeString(name) + " (" + strconv.Itoa(categories[name]) + ")</option>")
	}
	b.WriteString("</select><select id=\"integrity\"><option value=\"\">Todas as integridades</option>")
	for _, integrity := range []Integrity{IntegrityGood, IntegrityLikely, IntegrityPartial, IntegrityDamaged, IntegrityUnverified} {
		if n := integrities[integrity]; n > 0 {
			b.WriteString("<option value=\"" + html.EscapeString(strings.ToLower(string(integrity))) + "\">" + html.EscapeString(string(integrity)) + " (" + strconv.Itoa(n) + ")</option>")
		}
	}
	b.WriteString("</select><select id=\"size\"><option value=\"0\">Qualquer tamanho</option><option value=\"1048576\">Acima de 1 MB</option><option value=\"10485760\">Acima de 10 MB</option><option value=\"104857600\">Acima de 100 MB</option></select><label><input id=\"duplicates\" type=\"checkbox\">Mostrar duplicados</label><span id=\"shown\" class=\"badge\"></span></div>")
	b.WriteString("<div id=\"grid\" class=\"grid\">")
	for _, item := range items {
		targetPath := item.Path
		if item.DuplicateOf != "" {
			targetPath = item.DuplicateOf
		}
		rel, err := filepath.Rel(outDir, targetPath)
		if err != nil {
			rel = targetPath
		}
		href := filepath.ToSlash(rel)
		cls := "warn"
		if item.Integrity == IntegrityGood || item.Integrity == IntegrityLikely {
			cls = "good"
		} else if item.Integrity == IntegrityDamaged {
			cls = "bad"
		}
		searchText := strings.ToLower(strings.Join([]string{filepath.Base(item.Path), item.OriginalName, item.OriginalPath, item.Extension, item.Category, string(item.Integrity)}, " "))
		b.WriteString("<article class=\"card result-card\" data-search=\"" + html.EscapeString(searchText) + "\" data-category=\"" + html.EscapeString(strings.ToLower(item.Category)) + "\" data-integrity=\"" + html.EscapeString(strings.ToLower(string(item.Integrity))) + "\" data-size=\"" + strconv.FormatInt(item.Size, 10) + "\" data-duplicate=\"" + strconv.FormatBool(item.DuplicateOf != "") + "\">")
		if item.Category == "Fotos" && item.DuplicateOf == "" {
			b.WriteString("<img class=\"thumb\" loading=\"lazy\" src=\"" + html.EscapeString(href) + "\" alt=\"prévia\">")
		} else if item.Category == "Vídeos" && item.DuplicateOf == "" {
			b.WriteString("<video class=\"media\" controls preload=\"metadata\" src=\"" + html.EscapeString(href) + "\"></video>")
		} else if item.Category == "Áudios" && item.DuplicateOf == "" {
			b.WriteString("<audio class=\"media\" controls preload=\"metadata\" src=\"" + html.EscapeString(href) + "\"></audio>")
		}
		label := filepath.Base(item.Path)
		if item.OriginalName != "" {
			label = item.OriginalName
		}
		b.WriteString("<div class=\"item-title\"><a href=\"" + html.EscapeString(href) + "\">" + html.EscapeString(label) + "</a></div>")
		if item.OriginalPath != "" {
			b.WriteString("<div class=\"muted\">Original: " + html.EscapeString(item.OriginalPath) + "</div>")
		}
		b.WriteString("<span class=\"badge\">" + html.EscapeString(item.Category) + "</span><span class=\"badge\">." + html.EscapeString(item.Extension) + "</span><div class=\"" + cls + "\">" + html.EscapeString(string(item.Integrity)) + "</div><div class=\"muted\">" + html.EscapeString(humanBytes(item.Size)) + " — " + html.EscapeString(item.Method) + "</div>")
		if item.Notes != "" {
			b.WriteString("<div class=\"muted\">" + html.EscapeString(item.Notes) + "</div>")
		}
		if item.DuplicateOf != "" {
			b.WriteString("<div class=\"muted\">Duplicado de: " + html.EscapeString(item.DuplicateOf) + "</div>")
		}
		b.WriteString("</article>")
	}
	b.WriteString("</div><div id=\"empty\" class=\"empty card\">Nenhum resultado corresponde aos filtros.</div><script>(function(){const cards=[...document.querySelectorAll('.result-card')],q=document.getElementById('search'),cat=document.getElementById('category'),integ=document.getElementById('integrity'),size=document.getElementById('size'),dups=document.getElementById('duplicates'),shown=document.getElementById('shown'),empty=document.getElementById('empty');function norm(v){return (v||'').toLocaleLowerCase('pt-BR').normalize('NFD').replace(/[\\u0300-\\u036f]/g,'')}function apply(){const term=norm(q.value),category=norm(cat.value),integrity=norm(integ.value),minimum=Number(size.value||0),showDups=dups.checked;let count=0;cards.forEach(c=>{const ok=(!term||norm(c.dataset.search).includes(term))&&(!category||norm(c.dataset.category)===category)&&(!integrity||norm(c.dataset.integrity)===integrity)&&(Number(c.dataset.size)>=minimum)&&(showDups||c.dataset.duplicate!=='true');c.classList.toggle('hidden',!ok);if(ok)count++});shown.textContent=count+' exibido(s)';empty.style.display=count?'none':'block'}[q,cat,integ,size,dups].forEach(e=>e.addEventListener('input',apply));apply()})();</script></body></html>")
	return os.WriteFile(filepath.Join(outDir, "RESULTADOS.html"), []byte(b.String()), 0o644)
}

func appendHistory(destination string, result Result, mode OperationMode, source string) {
	path := filepath.Join(destination, "HdRecover_HISTORICO.json")
	type historyEntry struct {
		When       time.Time     `json:"when"`
		Mode       OperationMode `json:"mode"`
		Source     string        `json:"source"`
		OutputDir  string        `json:"output_dir"`
		FilesFound int           `json:"files_found"`
		Duration   string        `json:"duration"`
	}
	var history []historyEntry
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &history)
	}
	history = append(history, historyEntry{When: time.Now(), Mode: mode, Source: source, OutputDir: result.OutputDir, FilesFound: result.FilesFound, Duration: result.Duration.String()})
	if len(history) > 100 {
		history = history[len(history)-100:]
	}
	data, _ := json.MarshalIndent(history, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

var _ = fmt.Sprintf
