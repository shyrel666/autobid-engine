"""近似视觉对照：把冻结目标 PNG 缩放到实际截图尺寸，按截图清单的区域统计超出容差（每通道 8/255）的像素占比。

注意：实际截图来自 Claude 桌面应用内置浏览器的 JPEG 截图（800x500 或 585x1266，非 DPR=2 PNG），
目标图需要缩放、JPEG 有压缩噪声、且字体渲染环境与 capture-manifest.json 不同，
因此本脚本的结果只用于定位差异区域，不满足 FRONTEND_REFACTOR_PLAN.md 第 10.3 节的像素对照门槛。
用法：python -X utf8 compare.py（在本目录执行）。
"""
import json
from pathlib import Path

from PIL import Image, ImageChops

HERE = Path(__file__).parent
DESIGN = HERE.parent / 'design'
TOL = 8
CASES = {
    'V01': '01-project-list.png', 'V02': '02-execution-workspace.png', 'V03': '03-model-settings.png',
    'V04': '04-execution-expanded.png', 'V05': '05-sidebar-collapsed.png', 'V06': '06-writing-workspace.png',
    'V07': '07-execution-mobile.png',
}


def main():
    manifest = {c['file']: c for c in json.loads((DESIGN / 'capture-manifest.json').read_text(encoding='utf8'))['captures']}
    out = {}
    for case, file in CASES.items():
        actual_path = HERE / f'{case}-actual.jpg'
        if not actual_path.exists():
            continue
        cap = manifest[file]
        actual = Image.open(actual_path).convert('RGB')
        expected = Image.open(DESIGN / file).convert('RGB').resize(actual.size, Image.LANCZOS)
        expected.save(HERE / f'{case}-expected.png')
        diff = ImageChops.difference(expected, actual)
        # 任一通道超出容差即算差异像素；差异图以红色叠加在实际截图上
        r_, g_, b_ = diff.split()
        any_mask = ImageChops.lighter(ImageChops.lighter(r_, g_), b_).point(lambda v: 255 if v > TOL else 0)
        overlay = actual.copy()
        red = Image.new('RGB', actual.size, (220, 38, 38))
        overlay.paste(red, mask=any_mask)
        overlay.save(HERE / f'{case}-diff.png')
        sx = actual.size[0] / cap['viewport']['width']
        sy = actual.size[1] / cap['viewport']['height']
        regions = {}
        for r in cap['regions']:
            if r['width'] <= 0 or r['height'] <= 0 or r['name'] == 'nav-item':
                continue
            box = (max(0, round(r['x'] * sx)), max(0, round(r['y'] * sy)),
                   min(actual.size[0], round((r['x'] + r['width']) * sx)), min(actual.size[1], round((r['y'] + r['height']) * sy)))
            crop = any_mask.crop(box)
            hist = crop.histogram()
            total = crop.size[0] * crop.size[1]
            regions[r['name']] = round(100 * hist[255] / total, 1) if total else None
        out[case] = regions
    (HERE / 'region-diff.json').write_text(json.dumps(out, ensure_ascii=False, indent=1), encoding='utf8')
    print(json.dumps(out, ensure_ascii=False, indent=1))


if __name__ == '__main__':
    main()
