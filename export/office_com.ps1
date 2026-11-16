# Export refresh: open a generated DOCX in WPS or Word through COM, update every table of contents and every page
# reference to a section bookmark (PAGEREF _Ref...) until the results stop changing, then save the DOCX and/or
# export a PDF with heading bookmarks.
# Run by export/office_windows.go with Windows PowerShell 5.1 (STA). The result is one UTF-8 JSON object written to
# -Result: {"ok":true,"passes":n,"indexes":k,"pagerefs":m,"toc":"...","version":"..."} or {"ok":false,"error":"..."}.
# COM servers are not children of this script: before writing the result it waits for the automation processes it
# started (command line contains /Automation or -Embedding) to exit and kills the ones that do not. Processes that
# existed before this run, such as documents the user opened or resident WPS add-in hosts, are never touched.
param(
    [string]$Prog,
    [string]$Image,
    [string]$In,
    [string]$Result,
    [string]$OutDocx = '',
    [string]$OutPdf = ''
)
$ErrorActionPreference = 'Stop'
$r = [ordered]@{ ok = $false; killed = 0 }

function AutomationProcs {
    @(Get-CimInstance Win32_Process -Filter ("Name='{0}'" -f $Image) -ErrorAction SilentlyContinue |
        Where-Object { [string]$_.CommandLine -match '/Automation|-Embedding' })
}

# Updates the PAGEREF fields that point at section bookmarks (score index, deviation table, appendices) and returns
# their count and results. The PAGEREF fields inside a table of contents point at _Toc bookmarks and are left to the
# TOC update.
function UpdatePageRefs($d) {
    $n = 0
    $text = New-Object Text.StringBuilder
    $total = $d.Fields.Count
    for ($i = 1; $i -le $total; $i++) {
        $f = $d.Fields.Item($i)
        if ($f.Type -ne 37) { continue } # wdFieldPageRef
        if ([string]$f.Code.Text -notmatch '_Ref\d') { continue }
        [void]$f.Update()
        $n++
        [void]$text.Append([string]$f.Result.Text).Append(',')
    }
    return @($n, $text.ToString())
}

$before = @(AutomationProcs | ForEach-Object { [int]$_.ProcessId })
$app = $null
$doc = $null
try {
    $app = New-Object -ComObject $Prog
    try { $app.Visible = $false } catch {}
    try { $app.DisplayAlerts = 0 } catch {}
    try { $app.AutomationSecurity = 3 } catch {} # msoAutomationSecurityForceDisable: never run macros
    # Build only: WPS reports Application.Name as "Microsoft Word", so the caller adds its own engine label.
    try { $r.version = [string]$app.Build } catch {}

    $doc = $app.Documents.Open($In, $false, $false, $false)
    $count = $doc.TablesOfContents.Count
    $prev = $null
    $passes = 0
    $refs = 0
    $toc = ''
    # Updating a TOC can change its own length and move headings to other pages, so repeat until stable.
    for ($i = 1; $i -le 5; $i++) {
        $passes = $i
        for ($k = 1; $k -le $count; $k++) { $doc.TablesOfContents.Item($k).Update() }
        try { $doc.Repaginate() } catch {}
        $pr = UpdatePageRefs $doc
        $refs = $pr[0]
        $toc = ''
        for ($k = 1; $k -le $count; $k++) { $toc += [string]$doc.TablesOfContents.Item($k).Range.Text }
        $cur = $toc + '|' + [string]$pr[1]
        if ($cur -eq $prev) { break }
        $prev = $cur
    }
    $r.passes = $passes
    $r.indexes = $count
    $r.pagerefs = $refs
    $r.toc = [string]$toc

    # The PDF is exported before the DOCX is saved. Saving makes WPS fill an empty Author with the signed-in WPS
    # account (measured on WPS 12.1: WPS_<digits>), and a PDF exported after that carries the account in /Author.
    # Exported first, the PDF has the author the generator wrote (the bidder) or none. Both files still come from
    # the same layout. The saved DOCX has its properties rewritten by the caller (export.SanitizeDOCX).
    if ($OutPdf) {
        # wdExportFormatPDF, print quality, whole document, content only, document properties, heading bookmarks,
        # structure tags, bitmap missing fonts, not PDF/A.
        $doc.ExportAsFixedFormat($OutPdf, 17, $false, 0, 0, 1, 1, 0, $true, $true, 1, $true, $true, $false)
    }
    if ($OutDocx) { $doc.SaveAs2($OutDocx, 16) } # wdFormatDocumentDefault (.docx)
    $r.ok = $true
} catch {
    $r.error = $_.Exception.Message
} finally {
    if ($doc) { try { $doc.Close(0) } catch {} }
    # Quit() without arguments: Quit(0) fails in PowerShell because the parameter is by-reference.
    if ($app) { try { $app.Quit() } catch { $r.quit_error = $_.Exception.Message } }
    foreach ($o in @($doc, $app)) {
        if ($o) { try { [void][Runtime.InteropServices.Marshal]::ReleaseComObject($o) } catch {} }
    }
    $doc = $null
    $app = $null
    [GC]::Collect()
    [GC]::WaitForPendingFinalizers()

    $deadline = (Get-Date).AddSeconds(15)
    do {
        $left = @(AutomationProcs | Where-Object { $before -notcontains [int]$_.ProcessId })
        if ($left.Count -eq 0) { break }
        Start-Sleep -Milliseconds 300
    } while ((Get-Date) -lt $deadline)
    foreach ($p in $left) {
        try { Stop-Process -Id $p.ProcessId -Force -ErrorAction Stop; $r.killed++ } catch {}
    }
    [IO.File]::WriteAllText($Result, ($r | ConvertTo-Json -Compress), (New-Object Text.UTF8Encoding $false))
}
