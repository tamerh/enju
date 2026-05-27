# Render the README figures: TikZ .tex -> PDF -> SVG + PNG.
# Requires: pdflatex, dvisvgm, pdftoppm (texlive + poppler-utils).
#
#   make           # build all figures (svg + png)
#   make clean     # remove LaTeX intermediates (keeps .svg/.png)

FIGS := workflow

SVGS := $(addsuffix .svg,$(FIGS))
PNGS := $(addsuffix .png,$(FIGS))

all: $(SVGS) $(PNGS)

%.pdf: %.tex
	pdflatex -interaction=nonstopmode -halt-on-error $< >/dev/null

%.svg: %.pdf
	dvisvgm --pdf $< -o $@

%.png: %.pdf
	pdftoppm -png -r 220 $< $*-tmp && mv $*-tmp-1.png $@

clean:
	rm -f *.aux *.log *.pdf *-tmp-*.png

.PHONY: all clean
