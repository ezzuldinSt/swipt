package raster

import (
	"math"
	"time"
)

// Rect is [x, y, width, height], as the JavaScript API writes it.
type Rect [4]float64

// Point is [x, y].
type Point [2]float64

func (r Rect) maxX() float64 {
	return r[0] + r[2]
}

func (r Rect) maxY() float64 {
	return r[1] + r[3]
}

func (r Rect) center() Point {
	return Point{r[0] + r[2]/2, r[1] + r[3]/2}
}

func (r Rect) offset(x, y float64) Rect {
	return Rect{r[0] + x, r[1] + y, r[2], r[3]}
}

func (r Rect) standard() Rect {
	if r[2] < 0 {
		r[0], r[2] = r[0]+r[2], -r[2]
	}
	if r[3] < 0 {
		r[1], r[3] = r[1]+r[3], -r[3]
	}
	return r
}

// intersect answers false where CGRectIntersection answers the null rectangle.
func intersect(a, b Rect) (Rect, bool) {
	a, b = a.standard(), b.standard()
	left := math.Max(a[0], b[0])
	top := math.Max(a[1], b[1])
	right := math.Min(a.maxX(), b.maxX())
	bottom := math.Min(a.maxY(), b.maxY())
	if right < left || bottom < top {
		return Rect{}, false
	}
	return Rect{left, top, right - left, bottom - top}, true
}

// rounded keeps the two decimals of every coordinate the API returns (SWRounded).
func rounded(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Round(value*100) / 100
}

func roundedPoint(p Point) Point {
	return Point{rounded(p[0]), rounded(p[1])}
}

func roundedRect(r Rect) Rect {
	r = r.standard()
	return Rect{rounded(r[0]), rounded(r[1]), rounded(r[2]), rounded(r[3])}
}

func roundedCenter(r Rect) Point {
	return roundedPoint(r.standard().center())
}

func nowMilliseconds() float64 {
	return float64(time.Now().UnixMicro()) / 1000
}

// transform is a CGAffineTransform: x' = a·x + c·y + tx, y' = b·x + d·y + ty.
type transform struct {
	a, b, c, d, tx, ty float64
}

func scaling(x, y float64) transform {
	return transform{a: x, d: y}
}

func translation(x, y float64) transform {
	return transform{a: 1, d: 1, tx: x, ty: y}
}

func rotation(angle float64) transform {
	sin, cos := math.Sincos(angle)
	return transform{a: cos, b: sin, c: -sin, d: cos}
}

// then applies t first and next after it (CGAffineTransformConcat(t, next)).
func (t transform) then(next transform) transform {
	return transform{
		a:  t.a*next.a + t.b*next.c,
		b:  t.a*next.b + t.b*next.d,
		c:  t.c*next.a + t.d*next.c,
		d:  t.c*next.b + t.d*next.d,
		tx: t.tx*next.a + t.ty*next.c + next.tx,
		ty: t.tx*next.b + t.ty*next.d + next.ty,
	}
}

func (t transform) inverse() transform {
	determinant := t.a*t.d - t.b*t.c
	if determinant == 0 {
		return t
	}
	return transform{
		a:  t.d / determinant,
		b:  -t.b / determinant,
		c:  -t.c / determinant,
		d:  t.a / determinant,
		tx: (t.c*t.ty - t.d*t.tx) / determinant,
		ty: (t.b*t.tx - t.a*t.ty) / determinant,
	}
}

func (t transform) point(p Point) Point {
	return Point{t.a*p[0] + t.c*p[1] + t.tx, t.b*p[0] + t.d*p[1] + t.ty}
}

// rect is the bounding box of the transformed rectangle (CGRectApplyAffineTransform).
func (t transform) rect(r Rect) Rect {
	r = r.standard()
	corners := [4]Point{
		t.point(Point{r[0], r[1]}), t.point(Point{r.maxX(), r[1]}),
		t.point(Point{r[0], r.maxY()}), t.point(Point{r.maxX(), r.maxY()}),
	}
	left, top, right, bottom := corners[0][0], corners[0][1], corners[0][0], corners[0][1]
	for _, corner := range corners[1:] {
		left, right = math.Min(left, corner[0]), math.Max(right, corner[0])
		top, bottom = math.Min(top, corner[1]), math.Max(bottom, corner[1])
	}
	return Rect{left, top, right - left, bottom - top}
}

func (t transform) scale() float64 {
	return 1 / math.Sqrt(math.Abs(t.a*t.d-t.b*t.c))
}
