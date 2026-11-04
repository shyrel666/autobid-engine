@echo off
rem 控制台切 UTF-8，否则本文件里的中文在 GBK 终端下是乱码。
chcp 65001 >nul 2>&1
rem AutoBid Engine 守护启动脚本（Windows）
rem
rem 用法：
rem   start.bat                  构建前端与后端，就绪后自动打开浏览器并前台运行（Ctrl-C 停止）
rem   start.bat --build-only     只构建，不启动
rem   start.bat --no-pause       出错后不等待按键（终端/CI 使用）
rem   start.bat --no-open        就绪后不自动打开浏览器
rem
rem 构建完成后它只做一件事：把 bin\autobid.exe 跑起来，进程退出后按退出码决定要不要再拉起。
rem
rem   0      用户正常停止     -> 退出循环
rem   75     程序请求重启     -> 立刻重跑（预留给页面“一键更新/回滚”）
rem   其他   崩溃             -> 退避后重跑（1->2->4…最多 60 秒）
rem
rem 构建、依赖安装与数据库连接配置都在这里完成；连接串优先读环境变量，
rem 也可写在 .env.local.bat（已被 Git 忽略）。构建失败直接退出，不进入重启循环。

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=bin\autobid.exe"
set "RESTART_CODE=75"
set "MAX_DELAY=60"

set "AB_BUILD_ONLY="
set "AB_NO_PAUSE="
set "AB_NO_OPEN="

:args
if "%~1"=="" goto setup
if /i "%~1"=="--build-only" (
    set "AB_BUILD_ONLY=1"
    shift /1
    goto args
)
if /i "%~1"=="--no-pause" (
    set "AB_NO_PAUSE=1"
    shift /1
    goto args
)
if /i "%~1"=="--no-open" (
    set "AB_NO_OPEN=1"
    shift /1
    goto args
)
if /i "%~1"=="--help" goto help
echo [autobid] 未知参数 "%~1"，用 start.bat --help 查看用法。 1>&2
goto fail

:help
echo 用法: start.bat [--build-only] [--no-pause] [--no-open]
echo 连接串配置在环境变量或 .env.local.bat 的 AUTOBID_DATABASE_URL。
exit /b 0

:setup
if not exist ".env.local.bat" goto check_config
call ".env.local.bat"
if errorlevel 1 (
    echo [autobid] 加载 .env.local.bat 失败。 1>&2
    goto fail
)

:check_config
if defined AB_BUILD_ONLY goto check_tools
if defined AUTOBID_DATABASE_URL goto check_tools
echo [autobid] 未配置 AUTOBID_DATABASE_URL。 1>&2
echo [autobid] 在本脚本旁创建 .env.local.bat 并加入你的连接串： 1>&2
echo   set "AUTOBID_DATABASE_URL=postgres://USER:PASSWORD@127.0.0.1:5432/autobid?sslmode=disable" 1>&2
echo [autobid] PostgreSQL 需已启动且数据库已存在。 1>&2
goto fail

:check_tools
where go.exe >nul 2>&1
if errorlevel 1 (
    echo [autobid] 未找到 Go，请安装 Go 1.24 或更高版本。 1>&2
    goto fail
)
where node.exe >nul 2>&1
if errorlevel 1 (
    echo [autobid] 未找到 Node.js，请安装 Node.js LTS。 1>&2
    goto fail
)
where npm.cmd >nul 2>&1
if errorlevel 1 (
    echo [autobid] 未找到 npm，请检查 Node.js 安装。 1>&2
    goto fail
)

echo [autobid] 构建前端工作台...
pushd "web"
if errorlevel 1 (
    echo [autobid] 无法进入 web 目录。 1>&2
    goto fail
)
if not exist "node_modules\.bin\vite.cmd" goto install_web
if exist "node_modules\.bin\tsc.cmd" goto build_web

:install_web
echo [autobid] 按 package-lock.json 安装前端依赖...
call npm ci
if errorlevel 1 (
    echo [autobid] 前端依赖安装失败，见上方错误。 1>&2
    goto fail
)

:build_web
call npm run build
if errorlevel 1 (
    echo [autobid] 前端构建失败，见上方错误。 1>&2
    goto fail
)
popd

echo [autobid] 构建服务端...
if not exist "bin" mkdir "bin"
go build -o "bin\autobid.exe" ./cmd/server
if errorlevel 1 (
    echo [autobid] 服务端构建失败，见上方错误。 1>&2
    echo [autobid] 若 AutoBid 正在运行，请先停止再重建。 1>&2
    goto fail
)
if defined AB_BUILD_ONLY (
    echo [autobid] 构建完成: bin\autobid.exe 与 web\dist
    exit /b 0
)

rem 本机运行组件（见 docs/LOCAL_RUNTIME.md）：已按 tools\mermaid-cli 安装 Mermaid-CLI 时自动使用，浏览器用本机 Chrome / Edge。
set "AB_MMDC=%CD%\tools\mermaid-cli\node_modules\@mermaid-js\mermaid-cli\src\cli.js"
if not defined AUTOBID_MMDC_PATH if exist "%AB_MMDC%" set "AUTOBID_MMDC_PATH=%AB_MMDC%"
if not defined AUTOBID_MMDC_BROWSER if exist "%ProgramFiles%\Google\Chrome\Application\chrome.exe" set "AUTOBID_MMDC_BROWSER=%ProgramFiles%\Google\Chrome\Application\chrome.exe"
if not defined AUTOBID_MMDC_BROWSER if exist "%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe" set "AUTOBID_MMDC_BROWSER=%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe"

rem 本机 OCR 服务（PaddleOCR，见 docs/LOCAL_RUNTIME.md 3.3）：已按 tools\paddleocr 安装且下载模型时，在独立的最小化窗口中启动，
rem 只监听 127.0.0.1；服务端启动自检会等待它加载模型。已配置 AUTOBID_OCR_URL 或设置 AUTOBID_OCR_AUTOSTART=0 时不自动启动。
set "AB_OCR_PY=%CD%\tools\paddleocr\.venv\Scripts\python.exe"
if not defined AUTOBID_OCR_PORT set "AUTOBID_OCR_PORT=18765"
if defined AUTOBID_OCR_URL goto ocr_done
if "%AUTOBID_OCR_AUTOSTART%"=="0" goto ocr_done
if not exist "%AB_OCR_PY%" goto ocr_done
if not exist "%CD%\tools\paddleocr\models\PP-OCRv5_mobile_det\inference.pdiparams" goto ocr_done
echo [autobid] 启动本机 OCR 服务（PaddleOCR），端口 %AUTOBID_OCR_PORT% ...
start "autobid-ocr" /min "%AB_OCR_PY%" -X utf8 "%CD%\tools\paddleocr\service.py" --port %AUTOBID_OCR_PORT%
set "AUTOBID_OCR_URL=http://127.0.0.1:%AUTOBID_OCR_PORT%/ocr"
:ocr_done

if not defined AUTOBID_HTTP_ADDR set "AUTOBID_HTTP_ADDR=127.0.0.1:8080"
if not defined AUTOBID_WEB_DIR set "AUTOBID_WEB_DIR=%CD%\web\dist"
if not exist "%AUTOBID_WEB_DIR%\index.html" (
    echo [autobid] AUTOBID_WEB_DIR 缺少 index.html: %AUTOBID_WEB_DIR% 1>&2
    goto fail
)
set "AB_URL=http://%AUTOBID_HTTP_ADDR%"
if "%AUTOBID_HTTP_ADDR:~0,1%"==":" set "AB_URL=http://127.0.0.1%AUTOBID_HTTP_ADDR%"
if "%AUTOBID_HTTP_ADDR:~0,7%"=="0.0.0.0" set "AB_URL=http://127.0.0.1%AUTOBID_HTTP_ADDR:~7%"
echo [autobid] 启动 AutoBid Engine，监听 %AUTOBID_HTTP_ADDR% ...
echo [autobid] 服务地址 %AB_URL%
if not defined AB_NO_OPEN echo [autobid] 就绪后将自动打开浏览器（--no-open 可关闭）
echo [autobid] 按 Ctrl-C 停止；数据库迁移会在启动时自动执行。

rem 后台等待端口就绪后打开默认浏览器；只在首次启动时开一次，崩溃重启不会重复开页。
if not defined AB_NO_OPEN start "" /b powershell.exe -NoProfile -WindowStyle Hidden -Command "$u=[Uri]($env:AB_URL); for($i=0;$i -lt 60;$i++){ try { $c=New-Object Net.Sockets.TcpClient; $c.Connect($u.Host,$u.Port); $c.Close(); Start-Process ($u.AbsoluteUri); exit } catch { Start-Sleep -Seconds 1 } }"

set /a delay=1

:loop
"%BIN%"
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
    echo [autobid] 正常退出
    exit /b 0
)
rem Ctrl-C 在 Windows 上也可能以控制台中断码结束，视为正常停止。
if "!code!"=="-1073741510" (
    echo [autobid] 已停止
    exit /b 0
)
if "!code!"=="3221225786" (
    echo [autobid] 已停止
    exit /b 0
)
if "!code!"=="%RESTART_CODE%" (
    rem 更新/回滚已就绪：重跑后由新版本继续服务。
    echo [autobid] 请求重启...
    set /a delay=1
    goto loop
)

echo [autobid] 异常退出 ^(code=!code!^)，!delay!s 后重启 1>&2
rem timeout 在被重定向的控制台里会失败，用 ping 兜底（延时 N 秒需要 N+1 次）。
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop

:fail
if not defined AB_NO_PAUSE pause
exit /b 1
