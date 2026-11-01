# AutoBid export: refresh all indexes (TOC page numbers) of a DOCX in LibreOffice and save DOCX and/or PDF.
# Run with LibreOffice's bundled python. The Go side starts soffice with a private -env:UserInstallation profile
# and --accept=pipe,name=<pipe>;urp; this script connects to that pipe, so concurrent jobs never share a profile.
# Prints one JSON line to stdout: {"ok": true, "passes": n, "indexes": k, "toc": "..."} or {"ok": false, "error": "..."}.
import json
import sys
import time
import traceback

import uno
from com.sun.star.beans import PropertyValue


def prop(name, value):
    p = PropertyValue()
    p.Name = name
    p.Value = value
    return p


def connect(pipe, deadline):
    local = uno.getComponentContext()
    resolver = local.ServiceManager.createInstanceWithContext("com.sun.star.bridge.UnoUrlResolver", local)
    last = None
    while time.time() < deadline:
        try:
            return resolver.resolve("uno:pipe,name=%s;urp;StarOffice.ComponentContext" % pipe)
        except Exception as e:  # soffice is still starting
            last = e
            time.sleep(0.25)
    raise RuntimeError("cannot connect to soffice pipe %s: %s" % (pipe, last))


def index_text(doc):
    parts = []
    idx = doc.getDocumentIndexes()
    for i in range(idx.getCount()):
        parts.append(idx.getByIndex(i).getAnchor().getString())
    return "\n".join(parts)


def no_hanging_punctuation(doc):
    # LibreOffice's DOCX import ignores w:overflowPunct="0": full-width punctuation would hang past the right margin.
    fam = doc.getStyleFamilies().getByName("ParagraphStyles")
    for name in fam.getElementNames():
        try:
            fam.getByName(name).setPropertyValue("ParaIsHangingPunctuation", False)
        except Exception:
            pass

    def walk(text):
        en = text.createEnumeration()
        while en.hasMoreElements():
            el = en.nextElement()
            if el.supportsService("com.sun.star.text.Paragraph"):
                try:
                    el.setPropertyValue("ParaIsHangingPunctuation", False)
                except Exception:
                    pass
            elif el.supportsService("com.sun.star.text.TextTable"):
                for cell in el.getCellNames():
                    walk(el.getCellByName(cell))

    walk(doc.getText())


def main():
    args = dict(zip(sys.argv[1::2], sys.argv[2::2]))
    deadline = time.time() + float(args.get("--connect-timeout", "60"))
    ctx = connect(args["--pipe"], deadline)
    smgr = ctx.ServiceManager
    desktop = smgr.createInstanceWithContext("com.sun.star.frame.Desktop", ctx)
    doc = None
    try:
        url = uno.systemPathToFileUrl(args["--in"])
        doc = desktop.loadComponentFromURL(url, "_blank", 0, (prop("Hidden", True), prop("ReadOnly", False)))
        if doc is None:
            raise RuntimeError("LibreOffice could not load the document")
        no_hanging_punctuation(doc)
        # Updating the TOC can change its own length and push headings to other pages, so update until stable.
        passes, prev = 0, None
        for passes in range(1, 6):
            idx = doc.getDocumentIndexes()
            for i in range(idx.getCount()):
                idx.getByIndex(i).update()
            doc.getTextFields().refresh()
            doc.refresh()
            cur = index_text(doc)
            if cur == prev:
                break
            prev = cur
        out_docx = args.get("--out-docx")
        if out_docx:
            doc.storeToURL(uno.systemPathToFileUrl(out_docx), (prop("FilterName", "MS Word 2007 XML"), prop("Overwrite", True)))
        out_pdf = args.get("--out-pdf")
        if out_pdf:
            fd = uno.Any("[]com.sun.star.beans.PropertyValue", (prop("ExportBookmarks", True), prop("UseTaggedPDF", False)))
            doc.storeToURL(uno.systemPathToFileUrl(out_pdf), (prop("FilterName", "writer_pdf_Export"), prop("Overwrite", True), prop("FilterData", fd)))
        print(json.dumps({"ok": True, "passes": passes, "indexes": doc.getDocumentIndexes().getCount(), "toc": prev or ""}))
    finally:
        if doc is not None:
            try:
                doc.close(True)
            except Exception:
                pass
        try:
            desktop.terminate()
        except Exception:
            pass


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({"ok": False, "error": "%s: %s" % (type(e).__name__, e)}))
        sys.exit(2)
