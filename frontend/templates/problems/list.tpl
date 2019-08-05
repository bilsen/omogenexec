{{ define "problems_list" }}
<section>
  <article>
    <header class="article-header">
      <div class="row">
        <h1 class="display">Problemarkiv</h1>
      </div>
    </header>
    <div class="row">
      <table class="bordered" style="margin: auto; width: 100%">
      <thead>
        <tr>
          <th>ID</th>
          <th>Title</th>
        </tr>
      </thead>
			 {{range .D.Problems}}
        <tr>
          <td>{{ .ShortName }}</td>
          <td>
            <a href="{{ .Link }}">{{ .LocalizedTitle $.C.Locales }}</a>
          </td>
        </tr>
			 {{end}}
      </table>
    </div>
  </article>
</section>
{{ end }}
