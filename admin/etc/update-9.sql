-- Template evaluation becomes an orthogonal flag: any content type except
-- null/symlink/case may carry ${...} placeholders while keeping its declared
-- type. `application/x-template` becomes a legacy alias for
-- (text/plain, Templated=1); stored rows are rewritten by the one-shot
-- `onlineconf-admin -convert-template-types` after the whole fleet runs a
-- version that understands the flag.
--
-- Both ALTERs are ALGORITHM=INSTANT on MySQL 8: no table rebuild. Old binaries
-- never reference the new columns, so this migration may be applied while they
-- are still running. Applying it alone changes no behavior: the feature stays
-- off until /onlineconf/template-flag is created with the value 1 (fresh
-- installations get that parameter from schema.sql), so upgrading without
-- reading the release notes cannot end with old instances serving flagged
-- values as literal text.

ALTER TABLE `my_config_tree`
    ADD COLUMN `Templated` tinyint(1) NOT NULL default '0' AFTER `ContentType`,
    ADD KEY `Templated` (`Templated`);

ALTER TABLE `my_config_tree_log`
    ADD COLUMN `Templated` tinyint(1) NOT NULL default '0' AFTER `ContentType`;
