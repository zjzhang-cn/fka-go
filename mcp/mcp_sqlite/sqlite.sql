/*******************************************************************************
   本文件由 mcp_sqlite/obfuscate.mjs 生成（表名/列名/索引名为混淆后的短码）。
   seed=chinook-obf；源库=chinook.original.db。改回原名的映射只在这份生成器的语义字典里。
********************************************************************************/

CREATE TABLE [z_7c9787]
(
    [z_390399] INTEGER  NOT NULL,
    [z_e6d303] NVARCHAR(160)  NOT NULL,
    [z_2f7d91] INTEGER  NOT NULL,
    PRIMARY KEY  ([z_390399]),
    FOREIGN KEY ([z_2f7d91]) REFERENCES [z_f4a38f] ([z_2f7d91]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION
);
CREATE TABLE [z_f4a38f]
(
    [z_2f7d91] INTEGER  NOT NULL,
    [z_f77cd2] NVARCHAR(120),
    PRIMARY KEY  ([z_2f7d91])
);
CREATE TABLE [z_2af565]
(
    [z_8c8908] INTEGER  NOT NULL,
    [z_742f3a] NVARCHAR(40)  NOT NULL,
    [z_1aad66] NVARCHAR(20)  NOT NULL,
    [z_dd2680] NVARCHAR(80),
    [z_095c3b] NVARCHAR(70),
    [z_c89d56] NVARCHAR(40),
    [z_6e134e] NVARCHAR(40),
    [z_44584d] NVARCHAR(40),
    [z_a2c90a] NVARCHAR(10),
    [z_e2c0e1] NVARCHAR(24),
    [z_1c61a7] NVARCHAR(24),
    [z_bfe382] NVARCHAR(60)  NOT NULL,
    [z_a8691f] INTEGER,
    PRIMARY KEY  ([z_8c8908]),
    FOREIGN KEY ([z_a8691f]) REFERENCES [z_fbb450] ([z_05c212]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION
);
CREATE TABLE [z_fbb450]
(
    [z_05c212] INTEGER  NOT NULL,
    [z_1aad66] NVARCHAR(20)  NOT NULL,
    [z_742f3a] NVARCHAR(20)  NOT NULL,
    [z_e6d303] NVARCHAR(30),
    [z_4e96af] INTEGER,
    [z_5e1000] DATETIME,
    [z_c8bf4f] DATETIME,
    [z_095c3b] NVARCHAR(70),
    [z_c89d56] NVARCHAR(40),
    [z_6e134e] NVARCHAR(40),
    [z_44584d] NVARCHAR(40),
    [z_a2c90a] NVARCHAR(10),
    [z_e2c0e1] NVARCHAR(24),
    [z_1c61a7] NVARCHAR(24),
    [z_bfe382] NVARCHAR(60),
    PRIMARY KEY  ([z_05c212]),
    FOREIGN KEY ([z_4e96af]) REFERENCES [z_fbb450] ([z_05c212]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION
);
CREATE TABLE [z_69bb8b]
(
    [z_ccacba] INTEGER  NOT NULL,
    [z_f77cd2] NVARCHAR(120),
    PRIMARY KEY  ([z_ccacba])
);
CREATE TABLE [z_3ec2de]
(
    [z_a24fb2] INTEGER  NOT NULL,
    [z_8c8908] INTEGER  NOT NULL,
    [z_605bad] DATETIME  NOT NULL,
    [z_ead0f8] NVARCHAR(70),
    [z_3c9709] NVARCHAR(40),
    [z_ff3acc] NVARCHAR(40),
    [z_47e659] NVARCHAR(40),
    [z_4a113e] NVARCHAR(10),
    [z_6c577b] NUMERIC(10,2)  NOT NULL,
    PRIMARY KEY  ([z_a24fb2]),
    FOREIGN KEY ([z_8c8908]) REFERENCES [z_2af565] ([z_8c8908]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION
);
CREATE TABLE [z_3427e1]
(
    [z_9d0b0f] INTEGER  NOT NULL,
    [z_a24fb2] INTEGER  NOT NULL,
    [z_03ab46] INTEGER  NOT NULL,
    [z_aafe4d] NUMERIC(10,2)  NOT NULL,
    [z_9238bb] INTEGER  NOT NULL,
    PRIMARY KEY  ([z_9d0b0f]),
    FOREIGN KEY ([z_a24fb2]) REFERENCES [z_3ec2de] ([z_a24fb2]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION,
    FOREIGN KEY ([z_03ab46]) REFERENCES [z_fa92c4] ([z_03ab46]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION
);
CREATE TABLE [z_d3a8b1]
(
    [z_8c15c7] INTEGER  NOT NULL,
    [z_f77cd2] NVARCHAR(120),
    PRIMARY KEY  ([z_8c15c7])
);
CREATE TABLE [z_30d9c3]
(
    [z_ea1b0b] INTEGER  NOT NULL,
    [z_f77cd2] NVARCHAR(120),
    PRIMARY KEY  ([z_ea1b0b])
);
CREATE TABLE [z_7a1bbe]
(
    [z_ea1b0b] INTEGER  NOT NULL,
    [z_03ab46] INTEGER  NOT NULL,
    PRIMARY KEY  ([z_ea1b0b], [z_03ab46]),
    FOREIGN KEY ([z_ea1b0b]) REFERENCES [z_30d9c3] ([z_ea1b0b]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION,
    FOREIGN KEY ([z_03ab46]) REFERENCES [z_fa92c4] ([z_03ab46]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION
);
CREATE TABLE [z_fa92c4]
(
    [z_03ab46] INTEGER  NOT NULL,
    [z_f77cd2] NVARCHAR(200)  NOT NULL,
    [z_390399] INTEGER,
    [z_8c15c7] INTEGER  NOT NULL,
    [z_ccacba] INTEGER,
    [z_95237a] NVARCHAR(220),
    [z_a4e40b] INTEGER  NOT NULL,
    [z_0b3164] INTEGER,
    [z_aafe4d] NUMERIC(10,2)  NOT NULL,
    PRIMARY KEY  ([z_03ab46]),
    FOREIGN KEY ([z_390399]) REFERENCES [z_7c9787] ([z_390399]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION,
    FOREIGN KEY ([z_ccacba]) REFERENCES [z_69bb8b] ([z_ccacba]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION,
    FOREIGN KEY ([z_8c15c7]) REFERENCES [z_d3a8b1] ([z_8c15c7]) 
		ON DELETE NO ACTION ON UPDATE NO ACTION
);

CREATE INDEX [z_dbf834] ON [z_7c9787] ([z_2f7d91]);
CREATE INDEX [z_ed6567] ON [z_2af565] ([z_a8691f]);
CREATE INDEX [z_03f69d] ON [z_fbb450] ([z_4e96af]);
CREATE INDEX [z_787482] ON [z_3ec2de] ([z_8c8908]);
CREATE INDEX [z_ad2166] ON [z_3427e1] ([z_a24fb2]);
CREATE INDEX [z_929779] ON [z_3427e1] ([z_03ab46]);
CREATE INDEX [z_62bed7] ON [z_7a1bbe] ([z_ea1b0b]);
CREATE INDEX [z_e28b8c] ON [z_7a1bbe] ([z_03ab46]);
CREATE INDEX [z_960d9f] ON [z_fa92c4] ([z_390399]);
CREATE INDEX [z_932081] ON [z_fa92c4] ([z_ccacba]);
CREATE INDEX [z_29f0bb] ON [z_fa92c4] ([z_8c15c7]);
