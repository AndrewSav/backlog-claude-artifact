// Sort menu, direction button and filter box, shared by the backlog page
// (plugin/scripts/template.html, inlined at render time) and the server's
// project list (server/pages.go, embedded through assets.go). The page keeps
// its own storage and drawing; this only tracks the choice and applies it.
//
//   var ctl = listControls({
//     sortEl, dirEl, qEl,          // <select>, <button>, <input type=search>
//     sorts: { key: compare },     // ascending comparators, one per <option>
//     defaultSort: "updated",
//     descByDefault: ["updated"],  // sorts that start descending when picked
//     sort, desc,                  // saved choice, if any
//     onChange: function () {}     // after any change: save and redraw
//   });
//   ctl.sort, ctl.desc, ctl.q      // current choice (q is lower case)
//   ctl.apply(items, text, tie)    // items matching q, sorted; text(item) is what q searches
function listControls(o) {
  var c = { sort: o.sorts[o.sort] ? o.sort : o.defaultSort, q: "" };
  c.desc = typeof o.desc === "boolean" ? o.desc : o.descByDefault.indexOf(c.sort) >= 0;
  function paint() {
    o.dirEl.textContent = c.desc ? "↓" : "↑";
    o.dirEl.title = c.desc ? "Descending" : "Ascending";
  }
  o.sortEl.value = c.sort;
  o.sortEl.addEventListener("change", function () {
    c.sort = o.sorts[this.value] ? this.value : o.defaultSort;
    c.desc = o.descByDefault.indexOf(c.sort) >= 0;
    paint(); o.onChange();
  });
  o.dirEl.addEventListener("click", function () { c.desc = !c.desc; paint(); o.onChange(); });
  o.qEl.addEventListener("input", function () { c.q = this.value.trim().toLowerCase(); o.onChange(); });
  c.q = o.qEl.value.trim().toLowerCase();
  c.apply = function (items, text, tie) {
    var f = o.sorts[c.sort];
    return items.filter(function (it) { return !c.q || text(it).toLowerCase().indexOf(c.q) >= 0; })
      .sort(function (a, b) { var r = f(a, b) || (tie ? tie(a, b) : 0); return c.desc ? -r : r; });
  };
  paint();
  return c;
}
