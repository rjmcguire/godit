package main

import (
	"bytes"
	"fmt"
	"github.com/nsf/termbox-go"
	"github.com/nsf/tulib"
	"os"
	"unicode/utf8"
)

//----------------------------------------------------------------------------
// dirty flag
//----------------------------------------------------------------------------

type dirty_flag int

const (
	dirty_contents dirty_flag = (1 << iota)
	dirty_status

	dirty_everything = dirty_contents | dirty_status
)

//----------------------------------------------------------------------------
// view location
//
// This structure represents a view location in the buffer. It needs to be
// separated from the view, because it's also being saved by the buffer (in case
// if at the moment buffer has no views attached to it).
//----------------------------------------------------------------------------

type view_location struct {
	cursor       cursor_location
	top_line     *line
	top_line_num int

	// Various cursor offsets from the beginning of the line:
	// 1. in characters
	// 2. in visual cells
	// An example would be the '\t' character, which gives 1 character
	// offset, but 'tabstop_length' visual cells offset.
	cursor_coffset int
	cursor_voffset int

	// This offset is different from these three above, because it's the
	// amount of visual cells you need to skip, before starting to show the
	// contents of the cursor line. The value stays as long as the cursor is
	// within the same line. When cursor jumps from one line to another, the
	// value is recalculated. The logic behind this variable is somewhat
	// close to the one behind the 'top_line' variable.
	line_voffset int

	// this one is used for choosing the best location while traversing
	// vertically, every time 'cursor_voffset' changes due to horizontal
	// movement, this one must be changed as well
	last_cursor_voffset int
}

//----------------------------------------------------------------------------
// status reporter
//----------------------------------------------------------------------------

type status_reporter interface {
	set_status(format string, args ...interface{})
}

//----------------------------------------------------------------------------
// view
//
// Think of it as a window. It draws contents from a portion of a buffer into
// 'uibuf' and maintains things like cursor position.
//----------------------------------------------------------------------------

type view struct {
	view_location
	sr                  status_reporter
	status              bytes.Buffer // temporary buffer for status bar text
	buf                 *buffer      // currently displayed buffer
	uibuf               tulib.Buffer
	dirty               dirty_flag
	oneline             bool
	ac                  *autocompl
	last_vcommand_class vcommand_class
}

func new_view(sr status_reporter, buf *buffer) *view {
	v := new(view)
	v.sr = sr
	v.uibuf = tulib.NewBuffer(1, 1)
	v.attach(buf)
	return v
}

func (v *view) activate() {
	v.last_vcommand_class = vcommand_class_none
}

func (v *view) deactivate() {
	// on deactivation discard autocompl
	v.ac = nil
}

func (v *view) attach(b *buffer) {
	if v.buf == b {
		return
	}

	v.detach()
	v.buf = b
	v.view_location = b.loc
	b.add_view(v)
	v.dirty = dirty_everything
}

func (v *view) detach() {
	if v.buf != nil {
		v.buf.delete_view(v)
		v.buf = nil
	}
}

func (v *view) init_autocompl() {
	v.ac = new_autocompl(gocode_ac_func, v)
}

// Resize the 'v.uibuf', adjusting things accordingly.
func (v *view) resize(w, h int) {
	v.uibuf.Resize(w, h)
	v.adjust_line_voffset()
	v.adjust_top_line()
	v.dirty = dirty_everything
}

func (v *view) height() int {
	if !v.oneline {
		return v.uibuf.Height - 1
	}
	return v.uibuf.Height
}

func (v *view) vertical_threshold() int {
	max_v_threshold := (v.height() - 1) / 2
	if view_vertical_threshold > max_v_threshold {
		return max_v_threshold
	}
	return view_vertical_threshold
}

func (v *view) horizontal_threshold() int {
	max_h_threshold := (v.width() - 1) / 2
	if view_horizontal_threshold > max_h_threshold {
		return max_h_threshold
	}
	return view_horizontal_threshold
}

func (v *view) width() int {
	// TODO: perhaps if I want to draw line numbers, I will hack it there
	return v.uibuf.Width
}

// This function is similar to what happens inside 'draw', but it contains a
// certain amount of specific code related to 'loc.line_voffset'. You shouldn't
// use it directly, call 'draw' instead.
func (v *view) draw_cursor_line(line *line, coff int) {
	x := 0
	tabstop := 0
	linedata := line.data
	for {
		rx := x - v.line_voffset
		if len(linedata) == 0 {
			break
		}

		if x == tabstop {
			tabstop += tabstop_length
		}

		if rx >= v.uibuf.Width {
			last := coff + v.uibuf.Width - 1
			v.uibuf.Cells[last].Ch = '→'
			break
		}

		r, rlen := utf8.DecodeRune(linedata)
		if r == '\t' {
			// fill with spaces to the next tabstop
			for ; x < tabstop; x++ {
				rx := x - v.line_voffset
				if rx >= v.uibuf.Width {
					break
				}

				if rx >= 0 {
					v.uibuf.Cells[coff+rx].Ch = ' '
				}
			}
		} else {
			if rx >= 0 {
				v.uibuf.Cells[coff+rx].Ch = r
			}
			x++
		}
		linedata = linedata[rlen:]
	}

	if v.line_voffset != 0 {
		v.uibuf.Cells[coff].Ch = '←'
	}
}

func (v *view) draw_contents() {
	// clear the buffer
	v.uibuf.Fill(v.uibuf.Rect, termbox.Cell{
		Ch: ' ',
		Fg: termbox.ColorDefault,
		Bg: termbox.ColorDefault,
	})

	if v.uibuf.Width == 0 || v.uibuf.Height == 0 {
		return
	}

	// draw lines
	line := v.top_line
	coff := 0
	for y, h := 0, v.height(); y < h; y++ {
		if line == nil {
			break
		}

		if line == v.cursor.line {
			// special case, cursor line
			v.draw_cursor_line(line, coff)
			coff += v.uibuf.Width
			line = line.next
			continue
		}

		x := 0
		tabstop := 0
		linedata := line.data
		for {
			if len(linedata) == 0 {
				break
			}

			// advance tab stop to the next closest position
			if x == tabstop {
				tabstop += tabstop_length
			}

			if x >= v.uibuf.Width {
				last := coff + v.uibuf.Width - 1
				v.uibuf.Cells[last].Ch = '→'
				break
			}
			r, rlen := utf8.DecodeRune(linedata)
			if r == '\t' {
				// fill with spaces to the next tabstop
				for ; x < tabstop; x++ {
					if x >= v.uibuf.Width {
						break
					}

					v.uibuf.Cells[coff+x].Ch = ' '
				}
			} else {
				v.uibuf.Cells[coff+x].Ch = r
				x++
			}
			linedata = linedata[rlen:]
		}
		coff += v.uibuf.Width
		line = line.next
	}
}

func (v *view) draw_status() {
	if v.oneline {
		return
	}

	// draw status bar
	lp := tulib.DefaultLabelParams
	lp.Bg = termbox.AttrReverse
	lp.Fg = termbox.AttrReverse | termbox.AttrBold
	v.uibuf.Fill(tulib.Rect{0, v.height(), v.uibuf.Width, 1}, termbox.Cell{
		Fg: termbox.AttrReverse,
		Bg: termbox.AttrReverse,
		Ch: '─',
	})
	fmt.Fprintf(&v.status, "  %s  ", v.buf.name)
	v.uibuf.DrawLabel(tulib.Rect{3, v.height(), v.uibuf.Width, 1},
		&lp, v.status.Bytes())

	namel := v.status.Len()
	lp.Fg = termbox.AttrReverse
	v.status.Reset()
	fmt.Fprintf(&v.status, "(%d, %d)  ", v.cursor.line_num, v.cursor_voffset)
	v.uibuf.DrawLabel(tulib.Rect{3 + namel, v.height(), v.uibuf.Width, 1},
		&lp, v.status.Bytes())
	v.status.Reset()
}

// Draw the current view to the 'v.uibuf'.
func (v *view) draw() {
	if v.dirty&dirty_contents != 0 {
		v.dirty &^= dirty_contents
		v.draw_contents()
	}

	if v.dirty&dirty_status != 0 {
		v.dirty &^= dirty_status
		v.draw_status()
	}
}

// Move top line 'n' times forward or backward.
func (v *view) move_top_line_n_times(n int) {
	if n == 0 {
		return
	}

	top := v.top_line
	for top.prev != nil && n < 0 {
		top = top.prev
		v.top_line_num--
		n++
	}
	for top.next != nil && n > 0 {
		top = top.next
		v.top_line_num++
		n--
	}
	v.top_line = top
}

// Move cursor line 'n' times forward or backward.
func (v *view) move_cursor_line_n_times(n int) {
	if n == 0 {
		return
	}

	cursor := v.cursor.line
	for cursor.prev != nil && n < 0 {
		cursor = cursor.prev
		v.cursor.line_num--
		n++
	}
	for cursor.next != nil && n > 0 {
		cursor = cursor.next
		v.cursor.line_num++
		n--
	}
	v.cursor.line = cursor
}

// When 'top_line' was changed, call this function to possibly adjust the
// 'cursor_line'.
func (v *view) adjust_cursor_line() {
	vt := v.vertical_threshold()
	cursor := v.cursor.line
	co := v.cursor.line_num - v.top_line_num
	h := v.height()

	if cursor.next != nil && co < vt {
		v.move_cursor_line_n_times(vt - co)
	}

	if cursor.prev != nil && co >= h-vt {
		v.move_cursor_line_n_times((h - vt) - co - 1)
	}

	if cursor != v.cursor.line {
		cursor = v.cursor.line
		bo, co, vo := cursor.find_closest_offsets(v.last_cursor_voffset)
		v.cursor.boffset = bo
		v.cursor_coffset = co
		v.cursor_voffset = vo
		v.line_voffset = 0
		v.adjust_line_voffset()
		v.dirty = dirty_everything
	}
}

// When 'cursor_line' was changed, call this function to possibly adjust the
// 'top_line'.
func (v *view) adjust_top_line() {
	vt := v.vertical_threshold()
	top := v.top_line
	co := v.cursor.line_num - v.top_line_num
	h := v.height()

	if top.next != nil && co >= h-vt {
		v.move_top_line_n_times(co - (h - vt) + 1)
		v.dirty = dirty_everything
	}

	if top.prev != nil && co < vt {
		v.move_top_line_n_times(co - vt)
		v.dirty = dirty_everything
	}
}

// When 'cursor_voffset' was changed usually > 0, then call this function to
// possibly adjust 'line_voffset'.
func (v *view) adjust_line_voffset() {
	ht := v.horizontal_threshold()
	w := v.uibuf.Width
	vo := v.line_voffset
	cvo := v.cursor_voffset
	threshold := w - 1
	if vo != 0 {
		threshold -= ht - 1
	}

	if cvo-vo >= threshold {
		vo = cvo + (ht - w + 1)
	}

	if vo != 0 && cvo-vo < ht {
		vo = cvo - ht
		if vo < 0 {
			vo = 0
		}
	}

	if v.line_voffset != vo {
		v.line_voffset = vo
		v.dirty = dirty_everything
	}
}

func (v *view) cursor_position() (int, int) {
	y := v.cursor.line_num - v.top_line_num
	x := v.cursor_voffset - v.line_voffset
	return x, y
}

func (v *view) cursor_position_for(cursor cursor_location) (int, int) {
	y := cursor.line_num - v.top_line_num
	x := cursor.voffset() - v.line_voffset
	return x, y
}

// Move cursor to the 'boffset' position in the 'line'. Obviously 'line' must be
// from the attached buffer. If 'boffset' < 0, use 'last_cursor_voffset'. Keep
// in mind that there is no need to maintain connections between lines (e.g. for
// moving from a deleted line to another line).
func (v *view) move_cursor_to(c cursor_location) {
	v.dirty |= dirty_status
	if c.boffset < 0 {
		bo, co, vo := c.line.find_closest_offsets(v.last_cursor_voffset)
		v.cursor.boffset = bo
		v.cursor_coffset = co
		v.cursor_voffset = vo
	} else {
		vo, co := c.voffset_coffset()
		v.cursor.boffset = c.boffset
		v.cursor_coffset = co
		v.cursor_voffset = vo
	}
	if c.line == v.cursor.line {
		v.last_cursor_voffset = v.cursor_voffset
	} else {
		v.line_voffset = 0
	}
	v.cursor.line = c.line
	v.cursor.line_num = c.line_num
	v.adjust_line_voffset()
	v.adjust_top_line()

	if v.ac != nil {
		// update autocompletion on every cursor move
		ok := v.ac.update(v.cursor)
		if !ok {
			v.ac = nil
		}
	}
}

// Move cursor one character forward.
func (v *view) move_cursor_forward() {
	c := v.cursor
	if c.last_line() && c.eol() {
		v.sr.set_status("End of buffer")
		return
	}

	c.move_one_rune_forward()
	v.move_cursor_to(c)
}

// Move cursor one character backward.
func (v *view) move_cursor_backward() {
	c := v.cursor
	if c.first_line() && c.bol() {
		v.sr.set_status("Beginning of buffer")
		return
	}

	c.move_one_rune_backward()
	v.move_cursor_to(c)
}

// Move cursor to the next line.
func (v *view) move_cursor_next_line() {
	c := v.cursor
	if !c.last_line() {
		c = cursor_location{c.line.next, c.line_num + 1, -1}
		v.move_cursor_to(c)
	} else {
		v.sr.set_status("End of buffer")
	}
}

// Move cursor to the previous line.
func (v *view) move_cursor_prev_line() {
	c := v.cursor
	if !c.first_line() {
		c = cursor_location{c.line.prev, c.line_num - 1, -1}
		v.move_cursor_to(c)
	} else {
		v.sr.set_status("Beginning of buffer")
	}
}

// Move cursor to the beginning of the line.
func (v *view) move_cursor_beginning_of_line() {
	c := v.cursor
	c.move_beginning_of_line()
	v.move_cursor_to(c)
}

// Move cursor to the end of the line.
func (v *view) move_cursor_end_of_line() {
	c := v.cursor
	c.move_end_of_line()
	v.move_cursor_to(c)
}

// Move cursor to the beginning of the file (buffer).
func (v *view) move_cursor_beginning_of_file() {
	c := cursor_location{v.buf.first_line, 1, 0}
	v.move_cursor_to(c)
}

// Move cursor to the end of the file (buffer).
func (v *view) move_cursor_end_of_file() {
	c := cursor_location{v.buf.last_line, v.buf.lines_n, len(v.buf.last_line.data)}
	v.move_cursor_to(c)
}

// Move cursor to the end of the next (or current) word.
func (v *view) move_cursor_word_forward() {
	c := v.cursor
	ok := c.move_one_word_forward()
	v.move_cursor_to(c)
	if !ok {
		v.sr.set_status("End of buffer")
	}
}

func (v *view) move_cursor_word_backward() {
	c := v.cursor
	ok := c.move_one_word_backward()
	v.move_cursor_to(c)
	if !ok {
		v.sr.set_status("Beginning of buffer")
	}
}

// Move view 'n' lines forward or backward.
func (v *view) move_view_n_lines(n int) {
	prevtop := v.top_line_num
	v.move_top_line_n_times(n)
	v.adjust_cursor_line()
	if prevtop != v.top_line_num {
		v.dirty = dirty_everything
	}
}

// Check if it's possible to move view 'n' lines forward or backward.
func (v *view) can_move_top_line_n_times(n int) bool {
	if n == 0 {
		return true
	}

	top := v.top_line
	for top.prev != nil && n < 0 {
		top = top.prev
		n++
	}
	for top.next != nil && n > 0 {
		top = top.next
		n--
	}

	if n != 0 {
		return false
	}
	return true
}

// Move view 'n' lines forward or backward only if it's possible.
func (v *view) maybe_move_view_n_lines(n int) {
	if v.can_move_top_line_n_times(n) {
		v.move_view_n_lines(n)
	}
}

func (v *view) maybe_next_action_group() {
	b := v.buf
	if b.history.next == nil {
		// no need to move
		return
	}

	prev := b.history
	b.history = b.history.next
	b.history.prev = prev
	b.history.next = nil
	b.history.actions = nil
	b.history.before = v.cursor
}

func (v *view) finalize_action_group() {
	b := v.buf
	// finalize only if we're at the tip of the undo history, this function
	// will be called mainly after each cursor movement and actions alike
	// (that are supposed to finalize action group)
	if b.history.next == nil {
		b.history.next = new(action_group)
		b.history.after = v.cursor
	}
}

func (v *view) undo() {
	b := v.buf
	if b.history.prev == nil {
		// we're at the sentinel, no more things to undo
		return
	}

	// undo action causes finalization, always
	v.finalize_action_group()

	// undo invariant tells us 'len(b.history.actions) != 0' in case if this is
	// not a sentinel, revert the actions in the current action group
	for i := len(b.history.actions) - 1; i >= 0; i-- {
		a := &b.history.actions[i]
		a.revert(v)
	}
	v.move_cursor_to(b.history.before)
	v.last_cursor_voffset = v.cursor_voffset
	b.history = b.history.prev
}

func (v *view) redo() {
	b := v.buf
	if b.history.next == nil {
		// open group, obviously, can't move forward
		return
	}
	if len(b.history.next.actions) == 0 {
		// last finalized group, moving to the next group breaks the
		// invariant and doesn't make sense (nothing to redo)
		return
	}

	// move one entry forward, and redo all its actions
	b.history = b.history.next
	for i := range b.history.actions {
		a := &b.history.actions[i]
		a.apply(v)
	}
	v.move_cursor_to(b.history.after)
	v.last_cursor_voffset = v.cursor_voffset
}

func (v *view) action_insert(c cursor_location, data []byte) {
	v.maybe_next_action_group()
	a := action{
		what:   action_insert,
		data:   data,
		cursor: c,
		lines:  make([]*line, bytes.Count(data, []byte{'\n'})),
	}
	for i := range a.lines {
		a.lines[i] = new(line)
	}
	a.apply(v)
	v.buf.history.append(&a)
}

func (v *view) action_delete(c cursor_location, nbytes int) {
	v.maybe_next_action_group()
	d := c.extract_bytes(nbytes)
	a := action{
		what:   action_delete,
		data:   d,
		cursor: c,
		lines:  make([]*line, bytes.Count(d, []byte{'\n'})),
	}
	for i := range a.lines {
		a.lines[i] = c.line.next
		c.line = c.line.next
	}
	a.apply(v)
	v.buf.history.append(&a)
}

// Insert a rune 'r' at the current cursor position, advance cursor one character forward.
func (v *view) insert_rune(r rune) {
	var data [utf8.UTFMax]byte
	len := utf8.EncodeRune(data[:], r)
	c := v.cursor
	v.action_insert(c, data[:len])
	if r == '\n' {
		c.line = c.line.next
		c.line_num++
		c.boffset = 0
	} else {
		c.boffset += len
	}
	v.move_cursor_to(c)
	v.dirty = dirty_everything
}

// If at the beginning of the line, move contents of the current line to the end
// of the previous line. Otherwise, erase one character backward.
func (v *view) delete_rune_backward() {
	c := v.cursor
	if c.bol() {
		if c.first_line() {
			// beginning of the file
			v.sr.set_status("Beginning of buffer")
			return
		}
		c.line = c.line.prev
		c.line_num--
		c.boffset = len(c.line.data)
		v.action_delete(c, 1)
		v.move_cursor_to(c)
		v.dirty = dirty_everything
		return
	}

	_, rlen := c.rune_before()
	c.boffset -= rlen
	v.action_delete(c, rlen)
	v.move_cursor_to(c)
	v.dirty = dirty_everything
}

// If at the EOL, move contents of the next line to the end of the current line,
// erasing the next line after that. Otherwise, delete one character under the
// cursor.
func (v *view) delete_rune() {
	c := v.cursor
	if c.eol() {
		if c.last_line() {
			// end of the file
			v.sr.set_status("End of buffer")
			return
		}
		v.action_delete(c, 1)
		v.dirty = dirty_everything
		return
	}

	_, rlen := c.rune_under()
	v.action_delete(c, rlen)
	v.dirty = dirty_everything
}

// If not at the EOL, remove contents of the current line from the cursor to the
// end. Otherwise behave like 'delete'.
func (v *view) kill_line() {
	c := v.cursor
	if !c.eol() {
		// kill data from the cursor to the EOL
		v.action_delete(c, len(c.line.data)-c.boffset)
		v.dirty = dirty_everything
		return
	}
	v.delete_rune()
}

func (v *view) kill_word() {
	c1 := v.cursor
	c2 := c1
	c2.move_one_word_forward()
	d := c1.distance(c2)
	if d > 0 {
		v.action_delete(c1, d)
	}
}

func (v *view) kill_region() {
	if !v.buf.is_mark_set() {
		v.sr.set_status("The mark is not set now, so there is no region")
		return
	}

	c1 := v.cursor
	c2 := v.buf.mark
	d := c1.distance(c2)
	switch {
	case d == 0:
		return
	case d < 0:
		d = -d
		v.action_delete(c2, d)
		v.move_cursor_to(c2)
	default:
		v.action_delete(c1, d)
	}
}

func (v *view) set_mark() {
	v.buf.mark = v.cursor
	v.sr.set_status("Mark set")
}

func (v *view) swap_cursor_and_mark() {
	if v.buf.is_mark_set() {
		m := v.buf.mark
		v.buf.mark = v.cursor
		v.move_cursor_to(m)
	}
}

func (v *view) on_insert_adjust_top_line(a *action) {
	if a.cursor.line_num < v.top_line_num && len(a.lines) > 0 {
		// inserted one or more lines above the view
		v.top_line_num += len(a.lines)
		v.dirty |= dirty_status
	}
}

func (v *view) on_delete_adjust_top_line(a *action) {
	if a.cursor.line_num < v.top_line_num {
		// deletion above the top line
		if len(a.lines) == 0 {
			return
		}

		topnum := v.top_line_num
		first, last := a.deleted_lines()
		if first <= topnum && topnum <= last {
			// deleted the top line, adjust the pointers
			if a.cursor.line.next != nil {
				v.top_line = a.cursor.line.next
				v.top_line_num = a.cursor.line_num + 1
			} else {
				v.top_line = a.cursor.line
				v.top_line_num = a.cursor.line_num
			}
			v.dirty = dirty_everything
		} else {
			// no need to worry
			v.top_line_num -= len(a.lines)
			v.dirty |= dirty_status
		}
	}
}

func (v *view) on_insert(a *action) {
	v.on_insert_adjust_top_line(a)
	if v.top_line_num+v.height() <= a.cursor.line_num {
		// inserted something below the view, don't care
		return
	}
	if a.cursor.line_num < v.top_line_num {
		// inserted something above the top line
		if len(a.lines) > 0 {
			// inserted one or more lines, adjust line numbers
			v.cursor.line_num += len(a.lines)
			v.dirty |= dirty_status
		}
		return
	}
	c := v.cursor
	c.on_insert_adjust(a)
	v.move_cursor_to(c)
	v.last_cursor_voffset = v.cursor_voffset
	v.dirty = dirty_everything
}

func (v *view) on_delete(a *action) {
	v.on_delete_adjust_top_line(a)
	if v.top_line_num+v.height() <= a.cursor.line_num {
		// deleted something below the view, don't care
		return
	}
	if a.cursor.line_num < v.top_line_num {
		// deletion above the top line
		if len(a.lines) == 0 {
			return
		}

		_, last := a.deleted_lines()
		if last < v.top_line_num {
			// no need to worry
			v.cursor.line_num -= len(a.lines)
			v.dirty |= dirty_status
			return
		}
	}
	c := v.cursor
	c.on_delete_adjust(a)
	v.move_cursor_to(c)
	v.last_cursor_voffset = v.cursor_voffset
	v.dirty = dirty_everything
}

func (v *view) on_vcommand(cmd vcommand, arg rune) {
	class := cmd.class()
	if class != v.last_vcommand_class {
		v.last_vcommand_class = class
		v.finalize_action_group()
	}

	switch cmd {
	case vcommand_move_cursor_forward:
		v.move_cursor_forward()
	case vcommand_move_cursor_backward:
		v.move_cursor_backward()
	case vcommand_move_cursor_word_forward:
		v.move_cursor_word_forward()
	case vcommand_move_cursor_word_backward:
		v.move_cursor_word_backward()
	case vcommand_move_cursor_next_line:
		v.move_cursor_next_line()
	case vcommand_move_cursor_prev_line:
		v.move_cursor_prev_line()
	case vcommand_move_cursor_beginning_of_line:
		v.move_cursor_beginning_of_line()
	case vcommand_move_cursor_end_of_line:
		v.move_cursor_end_of_line()
	case vcommand_move_cursor_beginning_of_file:
		v.move_cursor_beginning_of_file()
	case vcommand_move_cursor_end_of_file:
		v.move_cursor_end_of_file()
	case vcommand_move_view_half_forward:
		v.maybe_move_view_n_lines(v.height() / 2)
	case vcommand_move_view_half_backward:
		v.move_view_n_lines(-v.height() / 2)
	case vcommand_set_mark:
		v.set_mark()
	case vcommand_swap_cursor_and_mark:
		v.swap_cursor_and_mark()
	case vcommand_insert_rune:
		v.insert_rune(arg)
	case vcommand_delete_rune_backward:
		v.delete_rune_backward()
	case vcommand_delete_rune:
		v.delete_rune()
	case vcommand_kill_line:
		v.kill_line()
	case vcommand_kill_word:
		v.kill_word()
	case vcommand_kill_region:
		v.kill_region()
	case vcommand_undo:
		v.undo()
	case vcommand_redo:
		v.redo()
	case vcommand_autocompl_init:
		v.init_autocompl()
	case vcommand_autocompl_finalize:
		v.ac.finalize(v)
		v.ac = nil
	case vcommand_autocompl_move_cursor_up:
		v.ac.move_cursor_up()
	case vcommand_autocompl_move_cursor_down:
		v.ac.move_cursor_down()
	}
}

func (v *view) on_key(ev *termbox.Event) {
	switch ev.Key {
	case termbox.KeyCtrlF, termbox.KeyArrowRight:
		v.on_vcommand(vcommand_move_cursor_forward, 0)
	case termbox.KeyCtrlB, termbox.KeyArrowLeft:
		v.on_vcommand(vcommand_move_cursor_backward, 0)
	case termbox.KeyCtrlN, termbox.KeyArrowDown:
		v.on_vcommand(vcommand_move_cursor_next_line, 0)
	case termbox.KeyCtrlP, termbox.KeyArrowUp:
		v.on_vcommand(vcommand_move_cursor_prev_line, 0)
	case termbox.KeyCtrlE, termbox.KeyEnd:
		v.on_vcommand(vcommand_move_cursor_end_of_line, 0)
	case termbox.KeyCtrlA, termbox.KeyHome:
		v.on_vcommand(vcommand_move_cursor_beginning_of_line, 0)
	case termbox.KeyCtrlV, termbox.KeyPgdn:
		v.on_vcommand(vcommand_move_view_half_forward, 0)
	case termbox.KeyCtrlSlash:
		v.on_vcommand(vcommand_undo, 0)
	case termbox.KeySpace:
		v.on_vcommand(vcommand_insert_rune, ' ')
	case termbox.KeyEnter, termbox.KeyCtrlJ:
		if v.ac != nil {
			v.on_vcommand(vcommand_autocompl_finalize, 0)
		} else {
			v.on_vcommand(vcommand_insert_rune, '\n')
		}
	case termbox.KeyBackspace, termbox.KeyBackspace2:
		v.on_vcommand(vcommand_delete_rune_backward, 0)
	case termbox.KeyDelete, termbox.KeyCtrlD:
		v.on_vcommand(vcommand_delete_rune, 0)
	case termbox.KeyCtrlK:
		v.on_vcommand(vcommand_kill_line, 0)
	case termbox.KeyPgup:
		v.on_vcommand(vcommand_move_view_half_backward, 0)
	case termbox.KeyCtrlR:
		v.on_vcommand(vcommand_redo, 0)
	case termbox.KeyTab:
		v.on_vcommand(vcommand_insert_rune, '\t')
	case termbox.KeyCtrlSpace:
		if ev.Ch == 0 {
			v.set_mark()
		}
	case termbox.KeyCtrlW:
		v.on_vcommand(vcommand_kill_region, 0)
	}

	if ev.Mod&termbox.ModAlt != 0 {
		switch ev.Ch {
		case 'v':
			v.on_vcommand(vcommand_move_view_half_backward, 0)
		case '<':
			v.on_vcommand(vcommand_move_cursor_beginning_of_file, 0)
		case '>':
			v.on_vcommand(vcommand_move_cursor_end_of_file, 0)
		case 'f':
			v.on_vcommand(vcommand_move_cursor_word_forward, 0)
		case 'b':
			v.on_vcommand(vcommand_move_cursor_word_backward, 0)
		case 'd':
			v.on_vcommand(vcommand_kill_word, 0)
		case 'n':
			if v.ac != nil {
				v.on_vcommand(vcommand_autocompl_move_cursor_down, 0)
			}
		case 'p':
			if v.ac != nil {
				v.on_vcommand(vcommand_autocompl_move_cursor_up, 0)
			}
		}
	} else if ev.Ch != 0 {
		v.on_vcommand(vcommand_insert_rune, ev.Ch)
	}
}

func (v *view) dump_info() {
	p := func(format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, format, args...)
	}

	p("Top line num: %d\n", v.top_line_num)
}

//----------------------------------------------------------------------------
// view commands
//----------------------------------------------------------------------------

type vcommand_class int

const (
	vcommand_class_none vcommand_class = iota
	vcommand_class_movement
	vcommand_class_insertion
	vcommand_class_deletion
	vcommand_class_history
	vcommand_class_misc
)

type vcommand int

const (
	// movement commands (finalize undo action group)
	_vcommand_movement_beg vcommand = iota
	vcommand_move_cursor_forward
	vcommand_move_cursor_backward
	vcommand_move_cursor_word_forward
	vcommand_move_cursor_word_backward
	vcommand_move_cursor_next_line
	vcommand_move_cursor_prev_line
	vcommand_move_cursor_beginning_of_line
	vcommand_move_cursor_end_of_line
	vcommand_move_cursor_beginning_of_file
	vcommand_move_cursor_end_of_file
	vcommand_move_view_half_forward
	vcommand_move_view_half_backward
	vcommand_set_mark
	vcommand_swap_cursor_and_mark
	_vcommand_movement_end

	// insertion commands
	_vcommand_insertion_beg
	vcommand_insert_rune
	_vcommand_insertion_end

	// deletion commands
	_vcommand_deletion_beg
	vcommand_delete_rune_backward
	vcommand_delete_rune
	vcommand_kill_line
	vcommand_kill_word
	vcommand_kill_region
	_vcommand_deletion_end

	// history commands (undo/redo)
	_vcommand_history_beg
	vcommand_undo
	vcommand_redo
	_vcommand_history_end

	// misc commands
	_vcommand_misc_beg
	vcommand_autocompl_init
	vcommand_autocompl_move_cursor_up
	vcommand_autocompl_move_cursor_down
	vcommand_autocompl_finalize
	_vcommand_misc_end
)

func (c vcommand) class() vcommand_class {
	switch {
	case c > _vcommand_movement_beg && c < _vcommand_movement_end:
		return vcommand_class_movement
	case c > _vcommand_insertion_beg && c < _vcommand_insertion_end:
		return vcommand_class_insertion
	case c > _vcommand_deletion_beg && c < _vcommand_deletion_end:
		return vcommand_class_deletion
	case c > _vcommand_history_beg && c < _vcommand_history_end:
		return vcommand_class_history
	case c > _vcommand_misc_beg && c < _vcommand_misc_end:
		return vcommand_class_misc
	}
	return vcommand_class_none
}
